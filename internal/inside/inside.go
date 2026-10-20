// Package inside is the workbench in the agent's container. The host hands it a folder with a
// git bundle of the target, the job spec and the agent's login; it works the ticket there, with
// the agent, its tests and git all inside the container, and leaves the results in another
// folder: the report, the traces, and the branch as a bundle (or a patch when the checks stayed
// red). Nothing of the host is mounted.
package inside

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alteralto/verstak/internal/job"
	"github.com/alteralto/verstak/internal/runner"
	"github.com/alteralto/verstak/internal/ticket"
	"github.com/alteralto/verstak/internal/workspace"
)

// Files of the hand-over.
const (
	SpecFile   = "job.json"
	BundleFile = "target.bundle"
	AuthFile   = "auth.json"
	ResultFile = "result.bundle"
	DiffFile   = "changes.diff"
	ReportJSON = "report.json"
)

// Spec is what the host asks for.
type Spec struct {
	Ticket ticket.Ticket      `json:"ticket"`
	Base   string             `json:"base"`
	Checks []string           `json:"checks"`
	Rounds int                `json:"rounds"`
	Model  string             `json:"model"`
	Effort string             `json:"effort"`
	Inbox  string             `json:"inbox"` // the host's repo the branch lands in, for the report
	Author workspace.Identity `json:"author"`
}

// Run works the ticket from in, in work, and leaves the results in out. codexHome receives the
// agent's login (the runner reads it from there); agent makes the runner for the spec's model.
func Run(ctx context.Context, in, work, out, codexHome string, agent func(Spec) runner.Runner) (job.Report, error) {
	var spec Spec
	b, err := os.ReadFile(filepath.Join(in, SpecFile))
	if err != nil {
		return job.Report{}, err
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return job.Report{}, fmt.Errorf("%s: %w", SpecFile, err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return job.Report{}, err
	}
	if auth, err := os.ReadFile(filepath.Join(in, AuthFile)); err == nil && codexHome != "" {
		if err := os.MkdirAll(codexHome, 0o700); err != nil {
			return job.Report{}, err
		}
		if err := os.WriteFile(filepath.Join(codexHome, AuthFile), auth, 0o600); err != nil {
			return job.Report{}, err
		}
	}
	cfg := job.Config{
		Work:   work,
		Rounds: max(spec.Rounds, 1),
		Targets: map[string]job.Target{"target": {
			Path: filepath.Join(in, BundleFile), Base: spec.Base, Checks: spec.Checks, Author: spec.Author,
		}},
	}
	rep, err := job.Run(ctx, cfg, "target", spec.Ticket, agent(spec))
	if err != nil {
		os.WriteFile(filepath.Join(out, "error.txt"), []byte(err.Error()+"\n"), 0o644)
		return rep, err
	}
	ws := &workspace.Workspace{Path: rep.Dir, Branch: rep.Branch, Base: rep.Base}
	switch rep.Status {
	case job.Done:
		if err := ws.Bundle(ctx, filepath.Join(out, ResultFile)); err != nil {
			return rep, err
		}
		if spec.Inbox != "" {
			rep.Fetch = fmt.Sprintf("git fetch %s %s", spec.Inbox, rep.Branch)
		}
	case job.Failed:
		if d, err := ws.Diff(ctx); err == nil && d != "" {
			os.WriteFile(filepath.Join(out, DiffFile), []byte(d+"\n"), 0o644)
		}
	}
	// The agent's own files, read here and copied as plain files: nothing of the clone's .git.
	own, _ := os.ReadDir(filepath.Join(rep.Dir, workspace.Dir))
	for _, e := range own {
		n := e.Name()
		if e.IsDir() || !(strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".jsonl")) {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(rep.Dir, workspace.Dir, n)); err == nil {
			os.WriteFile(filepath.Join(out, n), b, 0o644)
		}
	}
	// The report again, now with the host's way to fetch the branch.
	os.WriteFile(filepath.Join(out, "REPORT.md"), []byte(rep.Markdown()), 0o644)
	j, _ := json.MarshalIndent(rep, "", "  ")
	return rep, os.WriteFile(filepath.Join(out, ReportJSON), j, 0o644)
}

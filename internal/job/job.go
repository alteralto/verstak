// Package job takes one ticket through the workbench: a clean clone, the agent, our own checks,
// a follow-up round with the failures when they are red, a commit on the ticket's branch when
// they are green, and a report.
package job

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alteralto/verstak/internal/check"
	"github.com/alteralto/verstak/internal/runner"
	"github.com/alteralto/verstak/internal/ticket"
	"github.com/alteralto/verstak/internal/trace"
	"github.com/alteralto/verstak/internal/workspace"
	"github.com/alteralto/verstak/workplace"
)

// Config is verstak.json.
type Config struct {
	Work    string            `json:"work"`   // where the clones go
	Rounds  int               `json:"rounds"` // agent rounds per ticket, the first one included
	Codex   CodexConfig       `json:"codex"`
	Targets map[string]Target `json:"targets"`
}

type CodexConfig struct {
	Bin     string `json:"bin"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
	Network bool   `json:"network"`
}

// Target is a repo the agent may work on.
type Target struct {
	Path   string   `json:"path"`
	Base   string   `json:"base"`
	Checks []string `json:"checks"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Rounds < 1 {
		c.Rounds = 1
	}
	return c, nil
}

// Outcomes of a ticket.
const (
	Done      = "done"       // changed, checks green, committed on the branch
	Failed    = "failed"     // checks still red after the last round; changes left uncommitted
	NoChange  = "no-change"  // the agent changed nothing
	AgentDown = "agent-down" // the agent itself failed (no account, limit, crash)
)

// Report of a ticket.
type Report struct {
	Ticket  ticket.Ticket
	Target  string
	Status  string
	Rounds  int
	Summary string
	Checks  []check.Result
	Stat    string
	Commits string
	Usage   runner.Usage
	Took    time.Duration
	Dir     string
	Branch  string
	Err     string
}

// Run works one ticket on target with r. The report is also written to the workspace.
func Run(ctx context.Context, cfg Config, name string, tk ticket.Ticket, r runner.Runner) (Report, error) {
	start := time.Now()
	tg, ok := cfg.Targets[name]
	if !ok {
		return Report{}, fmt.Errorf("unknown target %q", name)
	}
	rep := Report{Ticket: tk, Target: name, Dir: filepath.Join(cfg.Work, tk.ID), Branch: "verstak/" + tk.ID}

	t0 := time.Now()
	ws, err := workspace.Prepare(ctx, tg.Path, tg.Base, rep.Dir, rep.Branch)
	if err != nil {
		return rep, err
	}
	tf, err := os.Create(ws.Own("trace.jsonl"))
	if err != nil {
		return rep, err
	}
	defer tf.Close()
	tr := trace.New(tf)
	tr.Span("clone", t0, nil, "target", name, "base", ws.Base)
	if err := setUp(ws, tk); err != nil {
		return rep, err
	}
	events, err := os.Create(ws.Own("agent.jsonl"))
	if err != nil {
		return rep, err
	}
	defer events.Close()

	// The agent's builds stay inside its clone: its sandbox writes nowhere else.
	env := []string{
		"GOCACHE=" + ws.Own(filepath.Join("cache", "go-build")),
		"GOTMPDIR=" + ws.Own("tmp"),
		"GOFLAGS=-mod=readonly",
		"GOPROXY=off",
	}
	for _, d := range []string{filepath.Join("cache", "go-build"), "tmp"} {
		os.MkdirAll(ws.Own(d), 0o755)
	}

	prompt, session := startPrompt, ""
	for round := 1; round <= cfg.Rounds; round++ {
		rep.Rounds = round
		t := time.Now()
		res, err := r.Run(ctx, runner.Job{
			Dir: ws.Path, Prompt: prompt, Session: session, Env: env, Log: events,
			Last: ws.Own(fmt.Sprintf("answer-%d.md", round)),
		})
		rep.Usage.Add(res.Usage)
		if res.Summary != "" {
			rep.Summary = res.Summary
		}
		session = res.Session
		tr.Span("agent", t, err, "round", round, "in", res.Usage.Input, "cached", res.Usage.Cached, "out", res.Usage.Output)
		if err != nil {
			rep.Status, rep.Err = AgentDown, err.Error()
			break
		}
		if changes, _ := ws.Changes(ctx); changes == "" {
			rep.Status = NoChange
			break
		}
		t = time.Now()
		rep.Checks = check.Run(ctx, ws.Path, tg.Checks, nil)
		passed := check.Passed(rep.Checks)
		tr.Span("checks", t, nil, "round", round, "passed", passed)
		if passed {
			rep.Status = Done
			break
		}
		rep.Status = Failed
		prompt = followUp(rep.Checks)
	}

	rep.Stat, _ = ws.Stat(ctx)
	if rep.Status == Done {
		if _, err := ws.Commit(ctx, commitMessage(tk)); err != nil {
			rep.Status, rep.Err = Failed, err.Error()
		}
	}
	rep.Commits, _ = ws.Commits(ctx)
	rep.Took = time.Since(start)
	tr.Span("ticket", start, nil, "status", rep.Status, "rounds", rep.Rounds)
	return rep, os.WriteFile(ws.Own("REPORT.md"), []byte(rep.Markdown()), 0o644)
}

// setUp puts the instructions, the persona and the ticket into the agent's folder.
func setUp(ws *workspace.Workspace, tk ticket.Ticket) error {
	err := fs.WalkDir(workplace.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := workplace.Files.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(ws.Own(p), b, 0o644)
	})
	if err != nil {
		return err
	}
	return os.WriteFile(ws.Own("TICKET.md"), []byte(tk.Markdown()), 0o644)
}

// startPrompt is short: the agent reads the rest itself, from files that link to each other.
const startPrompt = "Read .verstak/INSTRUCTIONS.md and follow it. Your task is in .verstak/TICKET.md."

func followUp(rs []check.Result) string {
	var b strings.Builder
	b.WriteString("The workbench ran the checks on your change and some are red. Fix the cause, not the test, " +
		"run the checks again yourself, update .verstak/NOTES.md, and finish with the same final message format.\n")
	for _, r := range rs {
		if r.OK {
			continue
		}
		fmt.Fprintf(&b, "\n### `%s` failed\n\n```\n%s\n```\n", r.Cmd, r.Output)
	}
	return b.String()
}

func commitMessage(tk ticket.Ticket) string {
	msg := "fix: " + tk.Title
	if tk.Body != "" {
		msg += "\n\n" + tk.Body
	}
	return msg
}

// Markdown is the report as the developer reads it.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", r.Ticket.Title)
	fmt.Fprintf(&b, "- Status: **%s** after %d round(s), %s\n", r.Status, r.Rounds, r.Took.Round(time.Second))
	fmt.Fprintf(&b, "- Target: %s, branch `%s` in `%s`\n", r.Target, r.Branch, r.Dir)
	fmt.Fprintf(&b, "- Tokens: %d in (%d cached), %d out\n", r.Usage.Input, r.Usage.Cached, r.Usage.Output)
	if r.Err != "" {
		fmt.Fprintf(&b, "- Error: %s\n", r.Err)
	}
	if len(r.Checks) > 0 {
		b.WriteString("\n## Checks\n\n")
		for _, c := range r.Checks {
			mark := "pass"
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(&b, "- %s `%s` (%s)\n", mark, c.Cmd, c.Took.Round(100*time.Millisecond))
		}
	}
	if r.Stat != "" {
		fmt.Fprintf(&b, "\n## Changes\n\n```\n%s\n```\n", r.Stat)
	}
	if r.Commits != "" {
		fmt.Fprintf(&b, "\nTake it into the target:\n\n```\ngit fetch %s %s\ngit log -p FETCH_HEAD\n```\n",
			filepath.ToSlash(r.Dir), r.Branch)
	}
	if r.Summary != "" {
		fmt.Fprintf(&b, "\n## The agent says\n\n%s\n", r.Summary)
	}
	return b.String()
}

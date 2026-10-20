package inside_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alteralto/verstak/internal/inside"
	"github.com/alteralto/verstak/internal/job"
	"github.com/alteralto/verstak/internal/runner"
	"github.com/alteralto/verstak/internal/ticket"
	"github.com/alteralto/verstak/internal/workspace"
)

type agent struct{ fix bool }

func (a agent) Run(_ context.Context, j runner.Job) (runner.Result, error) {
	body := "func Add(a, b int) int { return a * b }\n" // compiles, still red
	if a.fix {
		body = "func Add(a, b int) int { return a + b }\n"
	}
	os.WriteFile(filepath.Join(j.Dir, "add.go"), []byte("package sample\n\n"+body), 0o644)
	os.WriteFile(j.Last, []byte("## Summary\nfixed Add"), 0o644)
	return runner.Result{Summary: "## Summary\nfixed Add"}, nil
}

// handOver makes what the host gives the container: a bundle of a tiny buggy repo, the spec and
// a login.
func handOver(t *testing.T) (in string) {
	t.Helper()
	src, in := t.TempDir(), t.TempDir()
	write(t, src, "go.mod", "module sample\n\ngo 1.27.1\n")
	write(t, src, "add.go", "package sample\n\nfunc Add(a, b int) int { return a - b }\n")
	write(t, src, "add_test.go", "package sample\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"2+3\")\n\t}\n}\n")
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "Dev"},
		{"config", "user.email", "dev@example.com"}, {"add", "-A"}, {"commit", "-q", "-m", "start"}} {
		gitIn(t, src, a...)
	}
	if err := workspace.BundleOf(context.Background(), src, "main", filepath.Join(in, inside.BundleFile)); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(inside.Spec{
		Ticket: ticket.Ticket{ID: "20261021-020000-add", Title: "Add is wrong"},
		Base:   "main", Checks: []string{"go test ./..."}, Rounds: 1, Inbox: "F:/verstak-work/inbox.git",
		Author: workspace.Identity{Name: "Dev", Email: "dev@example.com"},
	})
	write(t, in, inside.SpecFile, string(spec))
	write(t, in, inside.AuthFile, `{"tokens":"not a real one"}`)
	return in
}

func TestDoneLeavesABundle(t *testing.T) {
	var echo strings.Builder
	in, work, out, home := handOver(t), t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "codex")
	rep, err := inside.Run(context.Background(), in, work, out, home, func(inside.Spec) runner.Runner { return agent{fix: true} }, &echo)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != job.Done {
		t.Fatalf("status %s: %+v %s", rep.Status, rep.Checks, rep.Err)
	}
	if !strings.Contains(echo.String(), `"span":"checks"`) {
		t.Errorf("the steps are not echoed as they happen:\n%s", echo.String())
	}
	for _, f := range []string{inside.ResultFile, "REPORT.md", inside.ReportJSON, "trace.jsonl", "agent.jsonl", "TICKET.md"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("no %s in out", f)
		}
	}
	report, _ := os.ReadFile(filepath.Join(out, "REPORT.md"))
	if !strings.Contains(string(report), "git fetch F:/verstak-work/inbox.git verstak/20261021-020000-add") {
		t.Errorf("the report does not say how to fetch from the host's inbox:\n%s", report)
	}
	// The host takes the branch from the bundle into its inbox.
	inbox := filepath.Join(t.TempDir(), "inbox.git")
	if err := workspace.Fetch(context.Background(), inbox, filepath.Join(out, inside.ResultFile), rep.Branch); err != nil {
		t.Fatal(err)
	}
	if msg := gitIn(t, inbox, "log", "-1", "--format=%s", rep.Branch); msg != "fix: Add is wrong" {
		t.Errorf("inbox branch head %q", msg)
	}
	if st, err := os.Stat(filepath.Join(home, inside.AuthFile)); err != nil || st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("login not handed to codex, or readable by others: %v", err)
	}
}

func TestRedLeavesAPatchNotABundle(t *testing.T) {
	in, work, out := handOver(t), t.TempDir(), t.TempDir()
	os.Remove(filepath.Join(in, inside.AuthFile)) // and no login: codex still gets a home
	home := filepath.Join(t.TempDir(), "codex")
	rep, err := inside.Run(context.Background(), in, work, out, home, func(inside.Spec) runner.Runner { return agent{} }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != job.Failed {
		t.Fatalf("status %s", rep.Status)
	}
	if _, err := os.Stat(filepath.Join(out, inside.ResultFile)); err == nil {
		t.Error("a bundle of red work")
	}
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		t.Errorf("no codex home without a login: %v", err)
	}
	d, err := os.ReadFile(filepath.Join(out, inside.DiffFile))
	if err != nil || !strings.Contains(string(d), "+func Add(a, b int) int { return a * b }") {
		t.Errorf("patch: %v\n%s", err, d)
	}
}

func write(t *testing.T, dir, name, s string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

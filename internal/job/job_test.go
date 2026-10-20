package job

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alteralto/verstak/internal/runner"
	"github.com/alteralto/verstak/internal/ticket"
)

// fake is an agent that edits files by a script, one step per round.
type fake struct {
	rounds  []func(dir string) // what each round does
	prompts []string
}

func (f *fake) Run(_ context.Context, j runner.Job) (runner.Result, error) {
	f.prompts = append(f.prompts, j.Prompt)
	i := len(f.prompts) - 1
	if i < len(f.rounds) && f.rounds[i] != nil {
		f.rounds[i](j.Dir)
	}
	answer := "## Summary\nround done\n\n## Open questions\nNone."
	os.WriteFile(j.Last, []byte(answer), 0o644)
	return runner.Result{Session: "s1", Summary: answer, Usage: runner.Usage{Input: 100, Cached: 60, Output: 10}}, nil
}

// target is a tiny Go repo with a bug: Add subtracts, and its test is red.
func target(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "go.mod", "module sample\n\ngo 1.27.1\n")
	write(t, dir, "add.go", "package sample\n\nfunc Add(a, b int) int { return a - b }\n")
	write(t, dir, "add_test.go", "package sample\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"2+2\")\n\t}\n}\n")
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "config", "user.name", "Dev")
	gitIn(t, dir, "config", "user.email", "dev@example.com")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "start")
	write(t, dir, "wip.txt", "not committed: must not reach the clone\n")
	return dir
}

func config(t *testing.T, src string, rounds int) Config {
	return Config{
		Work:   t.TempDir(),
		Rounds: rounds,
		Targets: map[string]Target{"sample": {
			Path: src, Base: "main", Checks: []string{"go vet ./...", "go test ./..."},
		}},
	}
}

var tk = ticket.Ticket{ID: "20261021-010000-add", Title: "Add subtracts", Body: "2+2 gives 0."}

func fix(dir string) {
	os.WriteFile(filepath.Join(dir, "add.go"), []byte("package sample\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644)
}

func TestFixedInOneRound(t *testing.T) {
	src := target(t)
	ag := &fake{rounds: []func(string){fix}}
	rep, err := Run(context.Background(), config(t, src, 2), "sample", tk, ag)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != Done || rep.Rounds != 1 {
		t.Fatalf("status %s after %d rounds, want done after 1", rep.Status, rep.Rounds)
	}
	if !strings.Contains(rep.Commits, "fix: Add subtracts") {
		t.Errorf("commits %q", rep.Commits)
	}
	author := gitIn(t, rep.Dir, "log", "-1", "--format=%an <%ae>")
	if author != "Dev <dev@example.com>" {
		t.Errorf("author %q: commits are the developer's", author)
	}
	if _, err := os.Stat(filepath.Join(rep.Dir, "wip.txt")); err == nil {
		t.Error("uncommitted work of the target reached the clone")
	}
	files := gitIn(t, rep.Dir, "show", "--name-only", "--format=", "HEAD")
	if files != "add.go" {
		t.Errorf("committed %q, want only add.go (.verstak stays out)", files)
	}
	if gitIn(t, rep.Dir, "remote") != "" {
		t.Error("the clone has a remote: a push could reach the target")
	}
	// The target is only read.
	if b := gitIn(t, src, "branch", "--list"); b != "* main" {
		t.Errorf("target branches %q", b)
	}
	if s := gitIn(t, src, "status", "--short"); s != "?? wip.txt" {
		t.Errorf("target status %q", s)
	}
	for _, f := range []string{"INSTRUCTIONS.md", "TICKET.md", "REPORT.md", "trace.jsonl"} {
		if _, err := os.Stat(filepath.Join(rep.Dir, ".verstak", f)); err != nil {
			t.Errorf("no .verstak/%s", f)
		}
	}
	report, _ := os.ReadFile(filepath.Join(rep.Dir, ".verstak", "REPORT.md"))
	if !strings.Contains(string(report), "git fetch") || !strings.Contains(string(report), "## Open questions") {
		t.Errorf("report:\n%s", report)
	}
}

func TestRedChecksGoBackToTheAgent(t *testing.T) {
	src := target(t)
	ag := &fake{rounds: []func(string){
		func(dir string) { // a wrong fix: compiles, still red
			os.WriteFile(filepath.Join(dir, "add.go"), []byte("package sample\n\nfunc Add(a, b int) int { return a * b + 1 }\n"), 0o644)
		},
		fix,
	}}
	rep, err := Run(context.Background(), config(t, src, 2), "sample", tk, ag)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != Done || rep.Rounds != 2 {
		t.Fatalf("status %s after %d rounds, want done after 2", rep.Status, rep.Rounds)
	}
	if !strings.Contains(ag.prompts[1], "go test ./...") || !strings.Contains(ag.prompts[1], "2+2") {
		t.Errorf("the follow-up does not show the failure:\n%s", ag.prompts[1])
	}
	if rep.Usage.Input != 200 {
		t.Errorf("usage %+v, want both rounds summed", rep.Usage)
	}
}

func TestStillRedIsNotCommitted(t *testing.T) {
	src := target(t)
	ag := &fake{rounds: []func(string){func(dir string) {
		os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("looked at it\n"), 0o644)
	}}}
	rep, err := Run(context.Background(), config(t, src, 1), "sample", tk, ag)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != Failed || rep.Commits != "" {
		t.Fatalf("status %s, commits %q: red checks must not be committed", rep.Status, rep.Commits)
	}
}

func TestNoChange(t *testing.T) {
	src := target(t)
	rep, err := Run(context.Background(), config(t, src, 2), "sample", tk, &fake{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != NoChange || rep.Rounds != 1 || len(rep.Checks) != 0 {
		t.Fatalf("status %s, rounds %d, checks %d", rep.Status, rep.Rounds, len(rep.Checks))
	}
}

func TestSecondTicketWithTheSameIDRefused(t *testing.T) {
	src := target(t)
	cfg := config(t, src, 1)
	if _, err := Run(context.Background(), cfg, "sample", tk, &fake{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), cfg, "sample", tk, &fake{}); err == nil {
		t.Fatal("a second run over the same workspace")
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

func TestDenyListWins(t *testing.T) {
	src := target(t)
	for _, deny := range []string{src, strings.ToUpper(src), filepath.Dir(src) + "/"} {
		cfg := config(t, src, 1)
		cfg.Deny = []string{"F:/somewhere-else", deny}
		ag := &fake{rounds: []func(string){fix}}
		if _, err := Run(context.Background(), cfg, "sample", tk, ag); err == nil {
			t.Errorf("deny %q: the target was worked on", deny)
		}
		if len(ag.prompts) != 0 {
			t.Errorf("deny %q: the agent ran", deny)
		}
		if entries, _ := os.ReadDir(cfg.Work); len(entries) != 0 {
			t.Errorf("deny %q: a clone was made", deny)
		}
	}
	// A sibling whose name only starts the same is not inside.
	if d := denied([]string{src}, src+"-other"); d != "" {
		t.Errorf("%s-other counted as inside %s", src, d)
	}
}

// The agent can write its clone's .git; a hook there must not run at our commit.
func TestHooksInTheCloneDoNotRun(t *testing.T) {
	src := target(t)
	ag := &fake{rounds: []func(string){func(dir string) {
		fix(dir)
		hook := "#!/bin/sh\necho ran > \"$(git rev-parse --show-toplevel)/../hook-ran\"\nexit 1\n"
		os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte(hook), 0o755)
		gitIn(t, dir, "config", "core.hooksPath", ".git/hooks") // and its own config points at it
	}}}
	cfg := config(t, src, 1)
	rep, err := Run(context.Background(), cfg, "sample", tk, ag)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status != Done {
		t.Fatalf("status %s (%s): the hook ran and blocked the commit", rep.Status, rep.Err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Work, "hook-ran")); err == nil {
		t.Fatal("the agent's pre-commit hook ran")
	}
}

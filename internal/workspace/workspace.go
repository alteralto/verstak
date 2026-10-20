// Package workspace gives each ticket its own clone of the target repo on its own branch. The
// target itself is only read: no branch, worktree or file of it is touched, and nothing is
// pushed. The developer fetches a finished branch from the clone when they like it.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Dir is the agent's own folder inside a clone; git ignores it (.git/info/exclude).
const Dir = ".verstak"

// Workspace is one clone.
type Workspace struct {
	Path   string
	Branch string
	Base   string // the commit the branch started from
}

// Identity is who commits; the developer, as in the target.
type Identity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// IdentityOf is the committer configured in repo.
func IdentityOf(ctx context.Context, repo string) Identity {
	name, _ := git(ctx, repo, "config", "user.name")
	email, _ := git(ctx, repo, "config", "user.email")
	return Identity{Name: name, Email: email}
}

// Prepare clones the committed state of base from src into path and starts branch there. Work
// in the target's folder that is not committed stays out of it. Commits are who's; an empty
// one is the committer of src (a bundle has none: pass it).
func Prepare(ctx context.Context, src, base, path, branch string, who Identity) (*Workspace, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("workspace %s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// Files as they are in the repo: with git's line-ending conversion on (the Windows default)
	// a fresh checkout already differs from the index, and every clone looks changed.
	if _, err := git(ctx, "", "clone", "--quiet", "--no-hardlinks", "--config", "core.autocrlf=false",
		"--branch", base, src, path); err != nil {
		return nil, err
	}
	w := &Workspace{Path: path, Branch: branch}
	steps := [][]string{
		{"checkout", "--quiet", "-b", branch},
		{"remote", "remove", "origin"}, // a push from here has nowhere to go
	}
	if who == (Identity{}) {
		who = IdentityOf(ctx, src)
	}
	if who.Name != "" {
		steps = append(steps, []string{"config", "user.name", who.Name})
	}
	if who.Email != "" {
		steps = append(steps, []string{"config", "user.email", who.Email})
	}
	for _, s := range steps {
		if _, err := git(ctx, path, s...); err != nil {
			return nil, err
		}
	}
	exclude := filepath.Join(path, ".git", "info", "exclude")
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "\n/%s/\n", Dir)
	f.Close()
	if err := os.MkdirAll(filepath.Join(path, Dir), 0o755); err != nil {
		return nil, err
	}
	head, err := git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	w.Base = head
	return w, nil
}

// Own is a path inside the agent's folder.
func (w *Workspace) Own(name string) string { return filepath.Join(w.Path, Dir, name) }

// Changes is `git status --short` of the clone: what the agent changed, committed or not, is
// in Stat.
func (w *Workspace) Changes(ctx context.Context) (string, error) {
	return git(ctx, w.Path, "status", "--short")
}

// Stat is the diff stat against the base, the working tree included.
func (w *Workspace) Stat(ctx context.Context) (string, error) {
	if _, err := git(ctx, w.Path, "add", "--all"); err != nil {
		return "", err
	}
	return git(ctx, w.Path, "diff", "--cached", "--stat", w.Base)
}

// Commit commits everything the agent left on its branch. Nothing to commit is not an error.
func (w *Workspace) Commit(ctx context.Context, msg string) (bool, error) {
	if _, err := git(ctx, w.Path, "add", "--all"); err != nil {
		return false, err
	}
	if s, err := git(ctx, w.Path, "status", "--porcelain"); err != nil || s == "" {
		return false, err
	}
	_, err := git(ctx, w.Path, "commit", "--quiet", "-m", msg)
	return err == nil, err
}

// Bundle writes the ticket's branch, with its history, to a git bundle file: the only way a
// result leaves the agent's container.
func (w *Workspace) Bundle(ctx context.Context, file string) error {
	_, err := git(ctx, w.Path, "bundle", "create", file, w.Branch)
	return err
}

// BundleOf writes ref of repo, with its history, to a bundle: how the target enters the agent's
// container. It only reads repo.
func BundleOf(ctx context.Context, repo, ref, file string) error {
	_, err := git(ctx, repo, "bundle", "create", file, ref)
	return err
}

// Fetch takes branch from a result bundle into repo (a bare inbox repo, made when missing). A
// bundle carries objects and refs only, never config or hooks.
func Fetch(ctx context.Context, repo, bundle, branch string) error {
	if _, err := os.Stat(repo); err != nil {
		if _, err := git(ctx, "", "init", "--quiet", "--bare", repo); err != nil {
			return err
		}
	}
	_, err := git(ctx, repo, "fetch", "--quiet", bundle, branch+":"+branch)
	return err
}

// Diff is everything changed since the base, the working tree included, as a patch.
func (w *Workspace) Diff(ctx context.Context) (string, error) {
	if _, err := git(ctx, w.Path, "add", "--all"); err != nil {
		return "", err
	}
	return git(ctx, w.Path, "diff", "--cached", w.Base)
}

// Commits since the base, one line each.
func (w *Workspace) Commits(ctx context.Context) (string, error) {
	return git(ctx, w.Path, "log", "--oneline", w.Base+"..HEAD")
}

// safe keeps the agent's clone from running anything at our git commands: its .git is the
// agent's to write, and hooks or an fsmonitor there would run with our rights. Hooks come from
// the null device, where no file can be.
var safe = []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(slices.Clone(safe), args...)...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

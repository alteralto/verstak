// Package runner hands a prompt to a coding agent working in a folder and waits for it. The
// agent writes code; it does not decide when the job is done (package check does).
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Job is one round: the first prompt of a ticket, or a follow-up on the same session.
type Job struct {
	Dir     string
	Prompt  string
	Session string    // set: continue this session (the agent keeps what it read)
	Last    string    // file for the agent's final message
	Env     []string  // added to the environment of the agent and its commands
	Log     io.Writer // the agent's raw event stream; nil drops it
}

// Usage in tokens, summed over the round.
type Usage struct {
	Input  int `json:"input_tokens"`
	Cached int `json:"cached_input_tokens"`
	Output int `json:"output_tokens"`
}

func (u *Usage) Add(o Usage) { u.Input += o.Input; u.Cached += o.Cached; u.Output += o.Output }

// Result of a round.
type Result struct {
	Summary string // the agent's final message
	Session string
	Usage   Usage
	Took    time.Duration
}

// Runner is a coding agent.
type Runner interface {
	Run(ctx context.Context, j Job) (Result, error)
}

// Codex runs `codex exec` on this machine in its workspace-write sandbox: it writes only inside
// the job's folder.
type Codex struct {
	Bin     string // "" finds one (FindCodex)
	Model   string
	Effort  string // model_reasoning_effort
	Network bool   // let the agent's commands reach the network
}

func (c Codex) Run(ctx context.Context, j Job) (Result, error) {
	start := time.Now()
	bin := c.Bin
	if bin == "" {
		var err error
		if bin, err = FindCodex(); err != nil {
			return Result{}, err
		}
	}
	args := []string{"exec"}
	if j.Session != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check", "-o", j.Last,
		"-c", `sandbox_mode="workspace-write"`)
	if c.Model != "" {
		args = append(args, "-m", c.Model)
	}
	if c.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", c.Effort))
	}
	if c.Network {
		args = append(args, "-c", "sandbox_workspace_write.network_access=true")
	}
	if j.Session != "" {
		args = append(args, j.Session)
	}
	// The prompt goes in through stdin: a long one does not fit a Windows command line, and an
	// open stdin with a prompt argument leaves exec waiting for more input.
	args = append(args, "-")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = j.Dir
	cmd.Env = append(os.Environ(), j.Env...)
	cmd.Stdin = strings.NewReader(j.Prompt)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	var src io.Reader = out
	if j.Log != nil {
		src = io.TeeReader(out, j.Log)
	}
	res := ReadEvents(src)
	err = cmd.Wait()
	res.Took = time.Since(start)
	if res.Session == "" {
		res.Session = j.Session
	}
	if b, rerr := os.ReadFile(j.Last); rerr == nil {
		res.Summary = strings.TrimSpace(string(b))
	}
	if err != nil {
		return res, fmt.Errorf("codex: %v: %s", err, lastLines(stderr.String(), 5))
	}
	return res, nil
}

// ReadEvents reads codex's JSON lines: the session id and the token usage of every turn.
func ReadEvents(r io.Reader) Result {
	var res Result
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Usage    *Usage `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.ThreadID != "" && res.Session == "" {
			res.Session = ev.ThreadID
		}
		if ev.Type == "turn.completed" && ev.Usage != nil {
			res.Usage.Add(*ev.Usage)
		}
	}
	return res
}

// FindCodex: the newest codex.exe of the Codex app (its CLI knows the newest models), then the
// codex on PATH.
func FindCodex() (string, error) {
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		found, _ := filepath.Glob(filepath.Join(dir, "OpenAI", "Codex", "bin", "*", "codex.exe"))
		slices.SortFunc(found, func(a, b string) int { return modTime(b).Compare(modTime(a)) })
		if len(found) > 0 {
			return found[0], nil
		}
	}
	if p, err := exec.LookPath("codex"); err == nil {
		return p, nil
	}
	return "", errors.New("codex not found: set codex.bin in the config")
}

func modTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " | ")
}

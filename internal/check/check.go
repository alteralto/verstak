// Package check runs a target's own checks (go vet, go test) after the agent is done. The verdict
// is ours, not the agent's: no "fixed" without green checks.
package check

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Result of one command.
type Result struct {
	Cmd    string
	OK     bool
	Output string // the tail of stdout and stderr together
	Took   time.Duration
}

// tailMax: the end of the output is where a failing test says why.
const tailMax = 6000

// Run runs each command in dir, all of them even after a failure, so the agent sees every
// problem at once. A command is split on spaces; there is no shell.
func Run(ctx context.Context, dir string, cmds []string, env []string) []Result {
	out := make([]Result, 0, len(cmds))
	for _, c := range cmds {
		f := strings.Fields(c)
		if len(f) == 0 {
			continue
		}
		start := time.Now()
		cmd := exec.CommandContext(ctx, f[0], f[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err := cmd.Run()
		s := buf.String()
		if err != nil && s == "" {
			s = err.Error()
		}
		out = append(out, Result{Cmd: c, OK: err == nil, Output: Tail(s, tailMax), Took: time.Since(start)})
	}
	return out
}

// Passed: every check is green (none at all counts as a pass).
func Passed(rs []Result) bool {
	for _, r := range rs {
		if !r.OK {
			return false
		}
	}
	return true
}

// Tail keeps the last n bytes of s, starting at a line break when it cuts.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return "…\n" + s
}

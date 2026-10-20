// Package sandbox runs a ticket in the agent's Docker container. The host side only moves
// files: a bundle of the target and the spec go in with docker cp, the results come out the
// same way, and the branch lands in a bare inbox repo from its bundle. Everything that runs the
// agent's code (Codex, the tests, git over the clone) runs in the container, which has:
//
//   - no folder of the host mounted, only its own volume, removed after the ticket;
//   - a non-root user, no capabilities, no new privileges, a read-only root, limits on memory,
//     processes and CPU, no devices and no Docker socket;
//   - an internal network with no route outside; the egress proxy is the only way out, and only
//     to the allowed host names.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alteralto/verstak/internal/inside"
	"github.com/alteralto/verstak/internal/job"
	"github.com/alteralto/verstak/internal/ticket"
	"github.com/alteralto/verstak/internal/workspace"
)

// Paths in the container; /work is the ticket's volume, owned by the agent's user in the image.
const (
	user      = "10001"
	workMount = "/work"
	inDir     = workMount + "/in"
	outDir    = workMount + "/out"
	codexHome = workMount + "/codex"
	proxyPort = "3128"
)

// Box is the configured sandbox.
type Box struct {
	job.DockerConfig
	Log io.Writer // the container's output; nil drops it
}

// defaults fill what the config leaves out.
func (b *Box) defaults() {
	if b.Image == "" {
		b.Image = "verstak-agent:latest"
	}
	if b.Network == "" {
		b.Network = "verstak-net"
	}
	if b.Egress == "" {
		b.Egress = "verstak-egress"
	}
	if len(b.Allow) == 0 {
		b.Allow = []string{"chatgpt.com", ".openai.com"}
	}
	if b.Memory == "" {
		b.Memory = "6g"
	}
	if b.CPUs == "" {
		b.CPUs = "4"
	}
	if b.Pids == 0 {
		b.Pids = 512
	}
}

// Hardening is the flags every agent container gets. Probe checks them from the inside.
func (b *Box) Hardening() []string {
	b.defaults()
	proxy := "http://" + b.Egress + ":" + proxyPort
	return []string{
		"--user", user,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,nosuid,nodev,size=1g",
		"--memory", b.Memory, "--memory-swap", b.Memory,
		"--cpus", b.CPUs,
		"--pids-limit", fmt.Sprint(b.Pids),
		"--network", b.Network,
		"-e", "HTTPS_PROXY=" + proxy, "-e", "https_proxy=" + proxy,
		"-e", "HTTP_PROXY=" + proxy, "-e", "http_proxy=" + proxy,
		"-e", "NO_PROXY=", "-e", "CODEX_HOME=" + codexHome,
	}
}

// ensure makes the internal network and starts a fresh egress proxy with today's allow list:
// on the default bridge to reach out, and on the internal network for the agent.
func (b *Box) ensure(ctx context.Context) error {
	b.defaults()
	if _, err := docker(ctx, "network", "inspect", b.Network); err != nil {
		if _, err := docker(ctx, "network", "create", "--internal", b.Network); err != nil {
			return err
		}
	}
	docker(ctx, "rm", "-f", b.Egress)
	if _, err := docker(ctx, "run", "-d", "--name", b.Egress,
		"--user", user, "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
		"--memory", "128m", "--pids-limit", "64",
		b.Image, "egress", "-listen", ":"+proxyPort, "-allow", strings.Join(b.Allow, ",")); err != nil {
		return err
	}
	_, err := docker(ctx, "network", "connect", b.Network, b.Egress)
	return err
}

// Result of a ticket in the box.
type Result struct {
	Report job.Report
	Dir    string // on the host: in/, out/, the logs
	Inbox  string // the bare repo the branch landed in ("" when there is none)
}

// Run works tk on target name of cfg in a fresh container.
func (b *Box) Run(ctx context.Context, cfg job.Config, name string, tk ticket.Ticket) (Result, error) {
	b.defaults()
	tg, err := cfg.Target(name)
	if err != nil {
		return Result{}, err
	}
	res := Result{Dir: filepath.Join(cfg.Work, tk.ID)}
	if _, err := os.Stat(res.Dir); err == nil {
		return res, fmt.Errorf("%s already exists", res.Dir)
	}
	in := filepath.Join(res.Dir, "in")
	if err := os.MkdirAll(in, 0o755); err != nil {
		return res, err
	}
	if err := workspace.BundleOf(ctx, tg.Path, tg.Base, filepath.Join(in, inside.BundleFile)); err != nil {
		return res, err
	}
	inbox := filepath.ToSlash(filepath.Join(cfg.Work, "inbox.git"))
	who := tg.Author
	if who == (workspace.Identity{}) {
		who = workspace.IdentityOf(ctx, tg.Path)
	}
	spec, _ := json.MarshalIndent(inside.Spec{
		Ticket: tk, Base: tg.Base, Checks: tg.Checks, Rounds: cfg.Rounds,
		Model: cfg.Codex.Model, Effort: cfg.Codex.Effort, Inbox: inbox, Author: who,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(in, inside.SpecFile), spec, 0o644); err != nil {
		return res, err
	}
	// The login goes in with the rest and is removed from the host folder at once.
	authCopy := filepath.Join(in, inside.AuthFile)
	if b.Auth != "" {
		if a, err := os.ReadFile(b.Auth); err == nil {
			os.WriteFile(authCopy, a, 0o600)
		}
	}
	defer os.Remove(authCopy)

	if err := b.ensure(ctx); err != nil {
		return res, err
	}
	box := "verstak-" + tk.ID
	if _, err := docker(ctx, "volume", "create", box); err != nil {
		return res, err
	}
	// Removed whatever happens, with a context of their own: ctx may be the one that ran out.
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		docker(c, "rm", "-f", box)
		docker(c, "volume", "rm", "-f", box)
		docker(c, "rm", "-f", b.Egress)
	}()
	args := append([]string{"create", "--name", box, "-v", box + ":" + workMount}, b.Hardening()...)
	args = append(args, b.Image, "verstak", "-inside")
	if _, err := docker(ctx, args...); err != nil {
		return res, err
	}
	if _, err := docker(ctx, "cp", in+string(filepath.Separator)+".", box+":"+inDir); err != nil {
		return res, err
	}
	os.Remove(authCopy)

	run := ctx
	if b.Timeout != "" {
		d, err := time.ParseDuration(b.Timeout)
		if err != nil {
			return res, fmt.Errorf("docker.timeout: %w", err)
		}
		var cancel context.CancelFunc
		run, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	logf, _ := os.Create(filepath.Join(res.Dir, "container.log"))
	var sink io.Writer = io.Discard
	if logf != nil {
		defer logf.Close()
		sink = logf
	}
	if b.Log != nil {
		sink = io.MultiWriter(sink, b.Log)
	}
	start := exec.CommandContext(run, "docker", "start", "--attach", box)
	start.Stdout, start.Stderr = sink, sink
	runErr := start.Run()
	if run.Err() != nil {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		docker(c, "kill", box)
		cancel()
		runErr = fmt.Errorf("the ticket ran out of time (%s)", b.Timeout)
	}

	// Results out, whatever the exit: a red run has a report too.
	out := filepath.Join(res.Dir, "out")
	if _, err := docker(context.Background(), "cp", box+":"+outDir+"/.", out); err != nil && runErr == nil {
		runErr = err
	}
	if logs, err := docker(context.Background(), "logs", b.Egress); err == nil {
		os.WriteFile(filepath.Join(res.Dir, "egress.jsonl"), []byte(logs+"\n"), 0o644)
	}
	b.keepLogin(box)

	if j, err := os.ReadFile(filepath.Join(out, inside.ReportJSON)); err == nil {
		json.Unmarshal(j, &res.Report)
	}
	if bundle := filepath.Join(out, inside.ResultFile); fileExists(bundle) && res.Report.Branch != "" {
		if err := workspace.Fetch(ctx, inbox, bundle, res.Report.Branch); err != nil {
			return res, err
		}
		res.Inbox = inbox
	}
	if runErr != nil && res.Report.Status == "" {
		return res, runErr
	}
	return res, nil
}

// keepLogin takes back the agent's login if Codex refreshed it in the container: the old one
// may stop working after a refresh. It is the agent's own account, used only for this box.
func (b *Box) keepLogin(box string) {
	if b.Auth == "" {
		return
	}
	tmp := b.Auth + ".new"
	if _, err := docker(context.Background(), "cp", box+":"+codexHome+"/"+inside.AuthFile, tmp); err != nil {
		return
	}
	defer os.Remove(tmp)
	a, err := os.ReadFile(tmp)
	if err != nil || len(a) > 64<<10 || !json.Valid(a) {
		return
	}
	os.WriteFile(b.Auth, a, 0o600)
}

// Probe runs the isolation checks (verstak -inside-probe) in a container made exactly like a
// ticket's. It returns their report; an error when one of them failed.
func (b *Box) Probe(ctx context.Context) (string, error) {
	if err := b.ensure(ctx); err != nil {
		return "", err
	}
	defer docker(context.Background(), "rm", "-f", b.Egress)
	args := append([]string{"run", "--rm"}, b.Hardening()...)
	args = append(args, b.Image, "verstak", "-inside-probe", "-egress", b.Egress+":"+proxyPort)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New("docker " + args[0] + ": " + msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

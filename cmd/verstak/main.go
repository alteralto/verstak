// Command verstak works one ticket in the agent's Docker container:
//
//	verstak -target verstak tickets/own-tests.md   ("-" reads the ticket from stdin)
//	verstak -probe                                  checks the container's isolation
//
// It prints where the report is; the exit code is 0 only when the fix is done and its branch is
// in the inbox. In the container the same binary runs with -inside (and -inside-probe).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/alteralto/verstak/internal/inside"
	"github.com/alteralto/verstak/internal/job"
	"github.com/alteralto/verstak/internal/probe"
	"github.com/alteralto/verstak/internal/runner"
	"github.com/alteralto/verstak/internal/sandbox"
	"github.com/alteralto/verstak/internal/ticket"
)

func main() {
	cfgPath := flag.String("config", "verstak.json", "config file")
	target := flag.String("target", "", "target repo, a key of targets in the config")
	rounds := flag.Int("rounds", 0, "agent rounds, 0 = as in the config")
	doProbe := flag.Bool("probe", false, "check the isolation of the agent's container")
	unsafeHost := flag.Bool("unsafe-host", false, "run the agent and its tests right on this machine (debugging only, see docs/security.md)")
	in := flag.Bool("inside", false, "in the agent's container: work the handed-over ticket")
	inProbe := flag.Bool("inside-probe", false, "in the agent's container: try what the sandbox must not allow")
	egress := flag.String("egress", "", "with -inside-probe: the proxy's host:port")
	gofmtCheck := flag.Bool("gofmt", false, "a check: fail when gofmt would change a .go file here")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch {
	case *gofmtCheck:
		// gofmt -l lists the files it would change but exits 0 either way.
		out, err := exec.Command("gofmt", "-l", ".").CombinedOutput()
		fmt.Print(string(out))
		if err != nil || len(strings.TrimSpace(string(out))) > 0 {
			fmt.Println("not gofmt-ed: run gofmt -w on the files above")
			os.Exit(1)
		}
		return
	case *inProbe:
		ok := true
		for _, c := range probe.Run(*egress) {
			mark := "pass"
			if !c.OK {
				mark, ok = "FAIL", false
			}
			fmt.Printf("%s  %-40s %s\n", mark, c.Name, c.Note)
		}
		if !ok {
			os.Exit(1)
		}
		return
	case *in:
		rep, err := inside.Run(ctx, "/work/in", "/work/ws", "/work/out", os.Getenv("CODEX_HOME"),
			func(s inside.Spec) runner.Runner {
				// The container is the sandbox; codex's own would need privileges it does not have.
				return runner.Codex{Bin: "codex", Model: s.Model, Effort: s.Effort, Network: true, Sandbox: "danger-full-access"}
			}, os.Stdout)
		if err != nil {
			fail(err)
		}
		fmt.Printf("inside: %s after %d round(s)\n", rep.Status, rep.Rounds)
		return
	}

	cfg, err := job.LoadConfig(*cfgPath)
	if err != nil {
		fail(err)
	}
	box := &sandbox.Box{DockerConfig: cfg.Docker, Log: os.Stdout}
	if *doProbe {
		out, err := box.Probe(ctx)
		fmt.Print(out)
		if err != nil {
			fail(fmt.Errorf("isolation probe failed: %w", err))
		}
		return
	}
	if *target == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: verstak -target <name> <ticket.md | ->  |  verstak -probe")
		os.Exit(2)
	}
	if *rounds > 0 {
		cfg.Rounds = *rounds
	}
	var text []byte
	if flag.Arg(0) == "-" {
		text, err = io.ReadAll(os.Stdin)
	} else {
		text, err = os.ReadFile(flag.Arg(0))
	}
	if err != nil {
		fail(err)
	}
	tk, err := ticket.Parse(string(text), time.Now())
	if err != nil {
		fail(err)
	}
	fmt.Printf("verstak: %s -> %s\n", tk.Title, filepath.Join(cfg.Work, tk.ID))

	if *unsafeHost {
		ag := runner.Codex{Bin: cfg.Codex.Bin, Model: cfg.Codex.Model, Effort: cfg.Codex.Effort, Network: cfg.Codex.Network}
		rep, err := job.Run(ctx, cfg, *target, tk, ag)
		if err != nil {
			fail(err)
		}
		done(rep, filepath.Join(rep.Dir, ".verstak", "REPORT.md"))
		return
	}
	res, err := box.Run(ctx, cfg, *target, tk)
	if err != nil {
		fail(err)
	}
	done(res.Report, filepath.Join(res.Dir, "out", "REPORT.md"))
}

func done(rep job.Report, report string) {
	fmt.Printf("%s after %d round(s), %s\nreport: %s\n", rep.Status, rep.Rounds, rep.Took.Round(time.Second), report)
	if rep.Status != job.Done {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "verstak:", err)
	os.Exit(1)
}

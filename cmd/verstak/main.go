// Command verstak works one ticket: verstak -target verstak tickets/own-tests.md
// ("-" reads the ticket from stdin). It prints where the report is; the exit code is 0 only
// when the fix is done and committed on its branch.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/alteralto/verstak/internal/job"
	"github.com/alteralto/verstak/internal/runner"
	"github.com/alteralto/verstak/internal/ticket"
)

func main() {
	cfgPath := flag.String("config", "verstak.json", "config file")
	target := flag.String("target", "", "target repo, a key of targets in the config")
	rounds := flag.Int("rounds", 0, "agent rounds, 0 = as in the config")
	flag.Parse()
	if *target == "" || flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: verstak -target <name> <ticket.md | ->")
		os.Exit(2)
	}
	cfg, err := job.LoadConfig(*cfgPath)
	if err != nil {
		fail(err)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ag := runner.Codex{Bin: cfg.Codex.Bin, Model: cfg.Codex.Model, Effort: cfg.Codex.Effort, Network: cfg.Codex.Network}
	fmt.Printf("verstak: %s -> %s\n", tk.Title, filepath.Join(cfg.Work, tk.ID))
	rep, err := job.Run(ctx, cfg, *target, tk, ag)
	if err != nil {
		fail(err)
	}
	fmt.Printf("%s after %d round(s), %s\nreport: %s\n", rep.Status, rep.Rounds, rep.Took.Round(time.Second),
		filepath.Join(rep.Dir, ".verstak", "REPORT.md"))
	if rep.Status != job.Done {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "verstak:", err)
	os.Exit(1)
}

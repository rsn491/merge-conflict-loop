// Command merge-conflict-loop rebases every open PR that has merge conflicts
// onto its base branch, lets a coding agent (pi + a local Ollama model)
// resolve the conflicts, and force-pushes the result.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/rsn491/merge-conflict-loop/internal/agent"
	"github.com/rsn491/merge-conflict-loop/internal/config"
	"github.com/rsn491/merge-conflict-loop/internal/gh"
	"github.com/rsn491/merge-conflict-loop/internal/orchestrator"
	"github.com/rsn491/merge-conflict-loop/internal/resolver"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Load(os.Args[1:])
	if err == flag.ErrHelp {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	workRoot, err := filepath.Abs(cfg.WorkRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	repoDir, err := filepath.Abs(cfg.RepoDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	fmt.Printf("merge-conflict-loop: repo=%s model=%s parallel=%d dry-run=%v\n", cfg.Repo, cfg.Model, cfg.MaxParallel, cfg.DryRun)

	pi := &agent.Pi{
		Cmd:        cfg.AgentCmd,
		Model:      cfg.Model,
		OllamaHost: cfg.OllamaHost,
		ConfigDir:  filepath.Join(workRoot, "pi-agent"),
		Timeout:    cfg.AgentTimeout,
	}
	if err := pi.Setup(); err != nil {
		fmt.Fprintln(os.Stderr, "error: writing pi config:", err)
		return 1
	}

	o := &orchestrator.Orchestrator{
		Opts: orchestrator.Options{
			Repo:        cfg.Repo,
			MaxParallel: cfg.MaxParallel,
			SkipDrafts:  cfg.SkipDrafts,
			SkipLabel:   cfg.SkipLabel,
			Comment:     cfg.Comment,
			DryRun:      cfg.DryRun,
			Out:         os.Stdout,
			SummaryPath: os.Getenv("GITHUB_STEP_SUMMARY"),
			GroupLogs:   os.Getenv("GITHUB_ACTIONS") == "true",
		},
		GitHub: gh.New(cfg.APIURL, cfg.Repo, cfg.Token),
		Resolver: &resolver.Resolver{
			Agent: pi,
			Opts: resolver.Options{
				RepoDir:      repoDir,
				Remote:       cfg.Remote,
				WorkRoot:     workRoot,
				MaxSteps:     cfg.MaxSteps,
				AgentRetries: cfg.AgentRetries,
				VerifyCmd:    cfg.VerifyCmd,
				DryRun:       cfg.DryRun,
				GitName:      cfg.GitName,
				GitEmail:     cfg.GitEmail,
			},
		},
	}

	prs, err := o.FindConflicting(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if cfg.ListOnly {
		for _, p := range prs {
			fmt.Printf("conflicting #%d %s -> %s\n", p.Number, p.Head.Ref, p.Base.Ref)
		}
		return 0
	}
	if len(prs) > 0 && !cfg.SkipPull {
		// Only touch Ollama when there is work, so no-op runs stay fast.
		if err := agent.EnsureModel(ctx, cfg.OllamaHost, cfg.Model, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	results := o.ResolveAll(ctx, prs)

	failed := 0
	for _, r := range results {
		if r.Status == resolver.Failed || r.Status == resolver.Unresolved {
			failed++
		}
	}
	fmt.Printf("done: %d PR(s) processed, %d not resolved\n", len(results), failed)
	if ctx.Err() != nil {
		return 1
	}
	if failed > 0 && cfg.Strict {
		return 2
	}
	return 0
}

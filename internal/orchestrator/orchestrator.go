// Package orchestrator finds conflicting PRs and resolves them concurrently.
package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsn491/merge-conflict-loop/internal/gh"
	"github.com/rsn491/merge-conflict-loop/internal/resolver"
)

// GitHub is the subset of the GitHub client the orchestrator needs.
type GitHub interface {
	ListOpenPRs(ctx context.Context) ([]gh.PR, error)
	ResolveMergeability(ctx context.Context, n int) (gh.PR, error)
	Comment(ctx context.Context, n int, body string) error
}

// PRResolver resolves a single PR.
type PRResolver interface {
	Resolve(ctx context.Context, pr resolver.PR) resolver.Result
}

// Options configures a run.
type Options struct {
	Repo        string
	MaxParallel int
	SkipDrafts  bool
	SkipLabel   string
	Comment     bool
	DryRun      bool
	Out         io.Writer // progress output
	SummaryPath string    // $GITHUB_STEP_SUMMARY, optional
	GroupLogs   bool      // wrap per-PR logs in ::group:: (GitHub Actions)
}

// Orchestrator ties the pieces together.
type Orchestrator struct {
	Opts     Options
	GitHub   GitHub
	Resolver PRResolver

	outMu sync.Mutex
}

func (o *Orchestrator) printf(format string, a ...any) {
	o.outMu.Lock()
	defer o.outMu.Unlock()
	fmt.Fprintf(o.Opts.Out, format, a...)
}

// FindConflicting returns open PRs that GitHub reports as conflicting
// (steps 1 and 2), excluding forks, opted-out and (optionally) draft PRs.
func (o *Orchestrator) FindConflicting(ctx context.Context) ([]gh.PR, error) {
	prs, err := o.GitHub.ListOpenPRs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}
	o.printf("found %d open PR(s)\n", len(prs))

	type res struct {
		pr  gh.PR
		err error
	}
	results := make([]res, len(prs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // polling mergeability is slow; do it in parallel
	for i, p := range prs {
		switch {
		case o.Opts.SkipLabel != "" && p.HasLabel(o.Opts.SkipLabel):
			o.printf("  #%d: skipped (label %q)\n", p.Number, o.Opts.SkipLabel)
			continue
		case o.Opts.SkipDrafts && p.Draft:
			o.printf("  #%d: skipped (draft)\n", p.Number)
			continue
		}
		wg.Add(1)
		go func(i int, n int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pr, err := o.GitHub.ResolveMergeability(ctx, n)
			results[i] = res{pr, err}
		}(i, p.Number)
	}
	wg.Wait()

	var out []gh.PR
	for _, r := range results {
		switch {
		case r.err != nil:
			o.printf("  warning: %v\n", r.err)
		case r.pr.Number == 0:
			// filtered above
		case r.pr.Mergeable == nil:
			o.printf("  #%d: mergeability still unknown, skipping\n", r.pr.Number)
		case !gh.IsConflicting(r.pr):
			// no conflicts
		case r.pr.IsFork():
			o.printf("  #%d: conflicting but from a fork, cannot push; skipping\n", r.pr.Number)
		default:
			o.printf("  #%d: conflicting (%s <- %s)\n", r.pr.Number, r.pr.Base.Ref, r.pr.Head.Ref)
			out = append(out, r.pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// Run finds conflicting PRs and resolves them asynchronously with at most
// MaxParallel in flight (step 3). It returns the per-PR results.
func (o *Orchestrator) Run(ctx context.Context) ([]resolver.Result, error) {
	prs, err := o.FindConflicting(ctx)
	if err != nil {
		return nil, err
	}
	return o.ResolveAll(ctx, prs), nil
}

// ResolveAll resolves the given PRs concurrently.
func (o *Orchestrator) ResolveAll(ctx context.Context, prs []gh.PR) []resolver.Result {
	if len(prs) == 0 {
		o.printf("no conflicting PRs, nothing to do\n")
		o.writeSummary(nil)
		return nil
	}
	o.printf("resolving %d PR(s), up to %d at a time\n", len(prs), o.Opts.MaxParallel)

	results := make([]resolver.Result, len(prs))
	sem := make(chan struct{}, max(1, o.Opts.MaxParallel))
	var wg sync.WaitGroup
	for i, p := range prs {
		wg.Add(1)
		go func(i int, p gh.PR) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = resolver.Result{PR: toPR(p), Status: resolver.Failed, Err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			o.printf("[#%d] started\n", p.Number)
			r := o.Resolver.Resolve(ctx, toPR(p))
			results[i] = r
			o.report(r)
			if o.Opts.Comment && !o.Opts.DryRun {
				if err := o.GitHub.Comment(context.Background(), p.Number, commentBody(r)); err != nil {
					o.printf("[#%d] warning: comment failed: %v\n", p.Number, err)
				}
			}
		}(i, p)
	}
	wg.Wait()
	o.writeSummary(results)
	return results
}

func toPR(p gh.PR) resolver.PR {
	return resolver.PR{Number: p.Number, Title: p.Title, Head: p.Head.Ref, Base: p.Base.Ref}
}

func (o *Orchestrator) report(r resolver.Result) {
	o.outMu.Lock()
	defer o.outMu.Unlock()
	w := o.Opts.Out
	msg := fmt.Sprintf("[#%d] %s in %s", r.PR.Number, r.Status, r.Duration.Round(time.Second))
	if r.Err != nil {
		msg += ": " + firstLine(r.Err.Error())
	}
	if o.Opts.GroupLogs {
		fmt.Fprintf(w, "::group::%s\n", msg)
		if b, err := os.ReadFile(r.LogPath); err == nil {
			w.Write(b)
		}
		fmt.Fprintln(w, "::endgroup::")
	} else {
		fmt.Fprintf(w, "%s (log: %s)\n", msg, r.LogPath)
	}
}

func (o *Orchestrator) writeSummary(results []resolver.Result) {
	if o.Opts.SummaryPath == "" {
		return
	}
	f, err := os.OpenFile(o.Opts.SummaryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, "## Merge conflict loop")
	if o.Opts.DryRun {
		fmt.Fprintln(f, "\n_Dry run: nothing was pushed._")
	}
	if len(results) == 0 {
		fmt.Fprintln(f, "\nNo conflicting PRs.")
		return
	}
	fmt.Fprintln(f, "\n| PR | Branch | Status | Steps | Time | Details |\n|---|---|---|---|---|---|")
	for _, r := range results {
		details := ""
		if r.Err != nil {
			details = strings.ReplaceAll(firstLine(r.Err.Error()), "|", "\\|")
		}
		fmt.Fprintf(f, "| #%d | `%s` → `%s` | %s | %d | %s | %s |\n",
			r.PR.Number, r.PR.Head, r.PR.Base, r.Status, r.Steps, r.Duration.Round(time.Second), details)
	}
}

func commentBody(r resolver.Result) string {
	var b strings.Builder
	switch r.Status {
	case resolver.Rebased:
		fmt.Fprintf(&b, "🤖 Rebased `%s` onto `%s` and resolved %d conflicting step(s) automatically. Please review the result.", r.PR.Head, r.PR.Base, r.Steps)
	case resolver.Skipped:
		return "🤖 Rebase produced no changes."
	default:
		fmt.Fprintf(&b, "🤖 Automatic conflict resolution for `%s` onto `%s` **%s**; the branch was left untouched and needs a manual rebase.", r.PR.Head, r.PR.Base, r.Status)
		if r.Err != nil {
			fmt.Fprintf(&b, "\n\n```\n%s\n```", truncate(r.Err.Error(), 1500))
		}
		if tail := logTail(r.LogPath, 30); tail != "" {
			fmt.Fprintf(&b, "\n\n<details><summary>Log tail</summary>\n\n```\n%s\n```\n</details>", tail)
		}
	}
	return b.String()
}

func logTail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return truncate(strings.Join(lines, "\n"), 4000)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

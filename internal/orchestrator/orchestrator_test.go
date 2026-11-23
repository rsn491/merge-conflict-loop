package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsn491/merge-conflict-loop/internal/gh"
	"github.com/rsn491/merge-conflict-loop/internal/resolver"
)

type fakeGH struct {
	prs      map[int]gh.PR
	mu       sync.Mutex
	comments map[int]string
}

func (f *fakeGH) ListOpenPRs(context.Context) ([]gh.PR, error) {
	var out []gh.PR
	for _, p := range f.prs {
		lite := p
		lite.Mergeable, lite.MergeableState = nil, "" // list endpoint omits it
		out = append(out, lite)
	}
	return out, nil
}

func (f *fakeGH) ResolveMergeability(_ context.Context, n int) (gh.PR, error) {
	return f.prs[n], nil
}

func (f *fakeGH) Comment(_ context.Context, n int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[n] = body
	return nil
}

type fakeResolver struct {
	inflight, peak atomic.Int32
	seen           sync.Map
}

func (f *fakeResolver) Resolve(_ context.Context, pr resolver.PR) resolver.Result {
	n := f.inflight.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(30 * time.Millisecond)
	f.inflight.Add(-1)
	f.seen.Store(pr.Number, true)
	st := resolver.Rebased
	if pr.Number == 4 {
		st = resolver.Unresolved
	}
	return resolver.Result{PR: pr, Status: st}
}

func pr(n int, mergeable bool, state string, mod func(*gh.PR)) gh.PR {
	repo := &struct {
		FullName string `json:"full_name"`
	}{"o/r"}
	p := gh.PR{Number: n, Title: "t", Mergeable: &mergeable, MergeableState: state,
		Head: gh.Ref{Ref: "b", Repo: repo}, Base: gh.Ref{Ref: "main", Repo: repo}}
	if mod != nil {
		mod(&p)
	}
	return p
}

func TestRunFiltersAndLimitsConcurrency(t *testing.T) {
	fg := &fakeGH{comments: map[int]string{}, prs: map[int]gh.PR{
		1: pr(1, false, "dirty", nil),
		2: pr(2, false, "dirty", nil),
		3: pr(3, false, "dirty", nil),
		4: pr(4, false, "dirty", nil),
		5: pr(5, true, "clean", nil), // no conflict
		6: pr(6, false, "dirty", func(p *gh.PR) {
			p.Head.Repo = &struct {
				FullName string `json:"full_name"`
			}{"fork/r"}
		}),
		7: pr(7, false, "dirty", func(p *gh.PR) { p.Labels = []gh.Label{{Name: "no-auto-rebase"}} }),
		8: pr(8, false, "dirty", func(p *gh.PR) { p.Draft = true }),
	}}
	fr := &fakeResolver{}
	summary := filepath.Join(t.TempDir(), "summary.md")
	var out bytes.Buffer
	o := &Orchestrator{GitHub: fg, Resolver: fr, Opts: Options{
		MaxParallel: 2, SkipLabel: "no-auto-rebase", SkipDrafts: true, Comment: true,
		Out: &out, SummaryPath: summary,
	}}
	results, err := o.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, r := range results {
		got = append(got, r.PR.Number)
	}
	if len(got) != 4 || got[0] != 1 || got[3] != 4 {
		t.Fatalf("resolved PRs = %v, want [1 2 3 4]\n%s", got, out.String())
	}
	if p := fr.peak.Load(); p != 2 {
		t.Errorf("peak concurrency = %d, want 2", p)
	}
	if len(fg.comments) != 4 || !strings.Contains(fg.comments[4], "needs a manual rebase") {
		t.Errorf("comments = %v", fg.comments)
	}
	b, _ := os.ReadFile(summary)
	if !strings.Contains(string(b), "| #4 |") || !strings.Contains(string(b), "unresolved") {
		t.Errorf("summary:\n%s", b)
	}
}

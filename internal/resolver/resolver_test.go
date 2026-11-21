package resolver

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rsn491/merge-conflict-loop/internal/agent"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture: a bare "origin" with main and a "feature" branch that conflicts
// with main on app.txt line 2. Returns (clone dir, bare dir).
func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	git(t, root, "init", "-q", "-b", "main", seed)
	write(t, filepath.Join(seed, "app.txt"), "line1\nvalue = 1\nline3\n")
	write(t, filepath.Join(seed, "other.txt"), "untouched\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-qm", "init")
	git(t, seed, "remote", "add", "origin", bare)
	git(t, seed, "push", "-q", "origin", "main")

	git(t, seed, "switch", "-qc", "feature")
	write(t, filepath.Join(seed, "app.txt"), "line1\nvalue = 1 + feature\nline3\n")
	git(t, seed, "commit", "-qam", "feature change")
	write(t, filepath.Join(seed, "feature.txt"), "new\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-qm", "feature file")
	git(t, seed, "push", "-q", "origin", "feature")

	git(t, seed, "switch", "-q", "main")
	write(t, filepath.Join(seed, "app.txt"), "line1\nvalue = 2\nline3\n")
	git(t, seed, "commit", "-qam", "main change")
	git(t, seed, "push", "-q", "origin", "main")

	clone := filepath.Join(root, "clone")
	git(t, root, "clone", "-q", bare, clone)
	return clone, bare
}

type funcAgent func(dir string, t agent.Task) error

func (f funcAgent) Resolve(_ context.Context, dir string, t agent.Task, _ io.Writer) error {
	return f(dir, t)
}

var conflictRe = regexp.MustCompile(`(?s)<<<<<<< [^\n]*\n.*?>>>>>>> [^\n]*\n`)

// goodAgent resolves the conflict by combining both sides.
func goodAgent(dir string, task agent.Task) error {
	for _, f := range task.Files {
		p := filepath.Join(dir, f)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := conflictRe.ReplaceAllString(string(b), "value = 2 + feature\n")
		if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func newResolver(t *testing.T, clone string, a agent.Agent) *Resolver {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	return &Resolver{Agent: a, Opts: Options{
		RepoDir: clone, Remote: "origin", WorkRoot: t.TempDir(),
		MaxSteps: 10, AgentRetries: 1, GitName: "bot", GitEmail: "bot@example.com",
	}}
}

var testPR = PR{Number: 7, Title: "Feature", Head: "feature", Base: "main"}

func TestResolveSuccess(t *testing.T) {
	clone, bare := fixture(t)
	before := git(t, bare, "rev-parse", "feature")
	r := newResolver(t, clone, funcAgent(goodAgent))

	res := r.Resolve(context.Background(), testPR)
	if res.Status != Rebased || !res.Pushed || res.Err != nil {
		t.Fatalf("got %+v", res)
	}
	if res.Steps != 1 {
		t.Errorf("steps = %d, want 1", res.Steps)
	}
	after := git(t, bare, "rev-parse", "feature")
	if after == before {
		t.Fatal("remote feature branch was not updated")
	}
	// feature is now on top of main and contains the merged content.
	git(t, bare, "merge-base", "--is-ancestor", "main", "feature")
	if got := git(t, bare, "show", "feature:app.txt"); got != "line1\nvalue = 2 + feature\nline3" {
		t.Errorf("app.txt = %q", got)
	}
	if got := git(t, bare, "show", "feature:feature.txt"); got != "new" {
		t.Errorf("feature.txt = %q", got)
	}
	if got := git(t, bare, "log", "-1", "--format=%an|%cn", "feature"); got != "t|bot" {
		t.Errorf("author|committer = %q, want t|bot", got)
	}
	if _, err := os.Stat(filepath.Join(r.Opts.WorkRoot, "pr-7")); !os.IsNotExist(err) {
		t.Errorf("worktree not cleaned up: %v", err)
	}
	if wts := git(t, clone, "worktree", "list"); strings.Count(wts, "\n") != 0 {
		t.Errorf("stale worktrees:\n%s", wts)
	}
}

func TestResolveDryRunDoesNotPush(t *testing.T) {
	clone, bare := fixture(t)
	before := git(t, bare, "rev-parse", "feature")
	r := newResolver(t, clone, funcAgent(goodAgent))
	r.Opts.DryRun = true
	res := r.Resolve(context.Background(), testPR)
	if res.Status != Rebased || res.Pushed {
		t.Fatalf("got %+v", res)
	}
	if git(t, bare, "rev-parse", "feature") != before {
		t.Fatal("dry run pushed")
	}
}

func TestResolveUnresolvedLeavesBranch(t *testing.T) {
	clone, bare := fixture(t)
	before := git(t, bare, "rev-parse", "feature")
	calls := 0
	lazy := funcAgent(func(string, agent.Task) error { calls++; return nil }) // leaves markers
	r := newResolver(t, clone, lazy)

	res := r.Resolve(context.Background(), testPR)
	if res.Status != Unresolved || !errors.Is(res.Err, ErrUnresolved) {
		t.Fatalf("got %+v", res)
	}
	if calls != 2 {
		t.Errorf("agent calls = %d, want 2 (1 + 1 retry)", calls)
	}
	if git(t, bare, "rev-parse", "feature") != before {
		t.Fatal("branch was pushed despite failure")
	}
}

func TestRetryGetsFailureReason(t *testing.T) {
	clone, _ := fixture(t)
	var prev []string
	a := funcAgent(func(dir string, task agent.Task) error {
		prev = append(prev, task.PrevFailure)
		if len(prev) == 1 {
			return nil // fail first, leaving markers
		}
		return goodAgent(dir, task)
	})
	res := newResolver(t, clone, a).Resolve(context.Background(), testPR)
	if res.Status != Rebased {
		t.Fatalf("got %+v", res)
	}
	if len(prev) != 2 || prev[0] != "" || !strings.Contains(prev[1], "conflict markers") {
		t.Errorf("prev failures = %q", prev)
	}
}

func TestStrayChangesAreReverted(t *testing.T) {
	clone, bare := fixture(t)
	a := funcAgent(func(dir string, task agent.Task) error {
		write(t, filepath.Join(dir, "other.txt"), "vandalised\n")
		write(t, filepath.Join(dir, "junk.tmp"), "x\n")
		return goodAgent(dir, task)
	})
	res := newResolver(t, clone, a).Resolve(context.Background(), testPR)
	if res.Status != Rebased {
		t.Fatalf("got %+v", res)
	}
	if got := git(t, bare, "show", "feature:other.txt"); got != "untouched" {
		t.Errorf("other.txt = %q", got)
	}
	if out := git(t, bare, "ls-tree", "--name-only", "feature"); strings.Contains(out, "junk.tmp") {
		t.Error("stray file was committed")
	}
}

func TestForceWithLeaseProtectsConcurrentPush(t *testing.T) {
	clone, bare := fixture(t)
	// While the agent works, someone else pushes to the PR branch.
	a := funcAgent(func(dir string, task agent.Task) error {
		other := filepath.Join(t.TempDir(), "other")
		git(t, filepath.Dir(other), "clone", "-q", "-b", "feature", bare, other)
		write(t, filepath.Join(other, "late.txt"), "late\n")
		git(t, other, "add", ".")
		git(t, other, "commit", "-qm", "late push")
		git(t, other, "push", "-q", "origin", "feature")
		return goodAgent(dir, task)
	})
	res := newResolver(t, clone, a).Resolve(context.Background(), testPR)
	if res.Status != Failed || res.Pushed {
		t.Fatalf("got %+v, want failed push", res)
	}
	if got := git(t, bare, "log", "-1", "--format=%s", "feature"); got != "late push" {
		t.Errorf("concurrent push was overwritten: head = %q", got)
	}
}

func TestVerifyCmdBlocksPush(t *testing.T) {
	clone, bare := fixture(t)
	before := git(t, bare, "rev-parse", "feature")
	r := newResolver(t, clone, funcAgent(goodAgent))
	r.Opts.VerifyCmd = "grep -q 'value = 3' app.txt"
	res := r.Resolve(context.Background(), testPR)
	if res.Status != Failed || git(t, bare, "rev-parse", "feature") != before {
		t.Fatalf("got %+v", res)
	}
}

func TestConcurrentResolves(t *testing.T) {
	clone, bare := fixture(t)
	// Second conflicting branch.
	seed := filepath.Join(t.TempDir(), "s")
	git(t, filepath.Dir(seed), "clone", "-q", bare, seed)
	git(t, seed, "switch", "-qc", "feature2", "origin/feature~2")
	write(t, filepath.Join(seed, "app.txt"), "line1\nvalue = 1 + feature\nline3\n")
	git(t, seed, "commit", "-qam", "feature2 change")
	git(t, seed, "push", "-q", "origin", "feature2")

	r := newResolver(t, clone, funcAgent(goodAgent))
	done := make(chan Result)
	for _, pr := range []PR{testPR, {Number: 8, Title: "F2", Head: "feature2", Base: "main"}} {
		go func(pr PR) { done <- r.Resolve(context.Background(), pr) }(pr)
	}
	for i := 0; i < 2; i++ {
		if res := <-done; res.Status != Rebased {
			t.Errorf("PR #%d: %+v", res.PR.Number, res)
		}
	}
}

func TestMarkerLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	write(t, p, "a\n<<<<<<< HEAD\nb\n=======\nc\n>>>>>>> abc (msg)\n<<<<<<<<not a marker\n")
	got, err := markerLines(p)
	if err != nil || len(got) != 2 || got[0] != 2 || got[1] != 6 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := markerLines(p + ".missing"); err != nil || got != nil {
		t.Fatalf("missing file: %v %v", got, err)
	}
}

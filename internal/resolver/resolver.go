// Package resolver rebases a single PR in an isolated git worktree, using an
// agent to resolve conflicts, and force-pushes the result.
package resolver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rsn491/merge-conflict-loop/internal/agent"
	"github.com/rsn491/merge-conflict-loop/internal/gitx"
)

// Status is the outcome of resolving one PR.
type Status string

const (
	Rebased    Status = "rebased"    // rebased and pushed (or would be, in dry-run)
	Unresolved Status = "unresolved" // the agent could not resolve the conflicts
	Failed     Status = "failed"     // infrastructure or verification error
	Skipped    Status = "skipped"    // nothing to do
)

// ErrUnresolved is returned when the agent cannot produce a clean resolution.
var ErrUnresolved = errors.New("conflicts could not be resolved")

// PR identifies the pull request to work on.
type PR struct {
	Number int
	Title  string
	Head   string // branch name
	Base   string // branch name
}

// Result describes what happened to a PR.
type Result struct {
	PR       PR
	Status   Status
	Pushed   bool
	Steps    int // conflicting rebase steps resolved by the agent
	Duration time.Duration
	LogPath  string
	Err      error
}

// Options configures a Resolver.
type Options struct {
	RepoDir      string
	Remote       string
	WorkRoot     string
	MaxSteps     int
	AgentRetries int
	VerifyCmd    string
	DryRun       bool
	GitName      string
	GitEmail     string
}

// Resolver resolves PRs. It is safe for concurrent use.
type Resolver struct {
	Opts  Options
	Agent agent.Agent

	// setupMu serialises operations on the shared repository (fetch,
	// worktree add/remove), which take locks in the common .git dir.
	setupMu sync.Mutex
}

// Resolve runs the full flow for one PR. It never returns an error; failures
// are reported in the Result.
func (r *Resolver) Resolve(ctx context.Context, pr PR) (res Result) {
	start := time.Now()
	res = Result{PR: pr, LogPath: filepath.Join(r.Opts.WorkRoot, fmt.Sprintf("pr-%d.log", pr.Number))}
	defer func() { res.Duration = time.Since(start) }()

	if err := os.MkdirAll(r.Opts.WorkRoot, 0o755); err != nil {
		res.Status, res.Err = Failed, err
		return res
	}
	logf, err := os.Create(res.LogPath)
	if err != nil {
		res.Status, res.Err = Failed, err
		return res
	}
	defer logf.Close()
	log := &syncWriter{w: logf}

	status, steps, pushed, err := r.run(ctx, pr, log)
	res.Status, res.Steps, res.Pushed, res.Err = status, steps, pushed, err
	if err != nil {
		fmt.Fprintf(log, "\nRESULT: %s: %v\n", status, err)
	} else {
		fmt.Fprintf(log, "\nRESULT: %s\n", status)
	}
	return res
}

func (r *Resolver) run(ctx context.Context, pr PR, log io.Writer) (Status, int, bool, error) {
	o := r.Opts
	remote := o.Remote
	repo := &gitx.Git{Dir: o.RepoDir, Log: log}
	wtPath := filepath.Join(o.WorkRoot, fmt.Sprintf("pr-%d", pr.Number))
	localBranch := fmt.Sprintf("merge-conflict-loop/pr-%d", pr.Number)
	baseRef := fmt.Sprintf("refs/remotes/%s/%s", remote, pr.Base)
	headRef := fmt.Sprintf("refs/remotes/%s/%s", remote, pr.Head)

	fmt.Fprintf(log, "PR #%d %q: rebase %s onto %s\n\n", pr.Number, pr.Title, pr.Head, pr.Base)

	// 3.1 Isolated workspace + 3.2 checkout of the PR branch.
	r.setupMu.Lock()
	err := func() error {
		if err := repo.Run(ctx, "fetch", "--no-tags", remote,
			fmt.Sprintf("+refs/heads/%s:%s", pr.Base, baseRef),
			fmt.Sprintf("+refs/heads/%s:%s", pr.Head, headRef)); err != nil {
			return err
		}
		r.removeWorktree(repo, wtPath, localBranch)
		return repo.Run(ctx, "worktree", "add", "-f", "-B", localBranch, wtPath, headRef)
	}()
	r.setupMu.Unlock()
	if err != nil {
		return Failed, 0, false, err
	}
	defer func() {
		r.setupMu.Lock()
		defer r.setupMu.Unlock()
		r.removeWorktree(repo, wtPath, localBranch)
	}()

	wt := &gitx.Git{Dir: wtPath, Log: log, Env: []string{
		"GIT_COMMITTER_NAME=" + o.GitName,
		"GIT_COMMITTER_EMAIL=" + o.GitEmail,
	}}
	origSHA, err := wt.Output(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Failed, 0, false, err
	}

	// 3.3 Rebase onto the target branch.
	rebaseErr := wt.Run(ctx, "-c", "rerere.enabled=false", "rebase", baseRef)
	if rebaseErr != nil && !wt.RebaseInProgress(ctx) {
		return Failed, 0, false, fmt.Errorf("rebase failed to start: %w", rebaseErr)
	}

	// 3.4 Let the agent resolve each conflicting step.
	steps := 0
	for wt.RebaseInProgress(ctx) {
		if err := ctx.Err(); err != nil {
			abort(wt)
			return Failed, steps, false, err
		}
		files, err := wt.Unmerged(ctx)
		if err != nil {
			abort(wt)
			return Failed, steps, false, err
		}
		if len(files) == 0 {
			if err := continueRebase(ctx, wt); err != nil {
				abort(wt)
				return Failed, steps, false, err
			}
			continue
		}
		if steps >= o.MaxSteps {
			abort(wt)
			return Unresolved, steps, false, fmt.Errorf("%w: exceeded %d conflicting steps", ErrUnresolved, o.MaxSteps)
		}
		steps++
		commit, _ := wt.Output(ctx, "log", "-1", "--format=%h %s%n%n%b", "REBASE_HEAD")
		task := agent.Task{PRNumber: pr.Number, PRTitle: pr.Title, Base: pr.Base, Head: pr.Head, Commit: commit, Files: files}

		var verr error
		for attempt := 0; attempt <= o.AgentRetries; attempt++ {
			fmt.Fprintf(log, "\n--- step %d, attempt %d: %d conflicted file(s): %s\n", steps, attempt+1, len(files), strings.Join(files, ", "))
			if verr = r.Agent.Resolve(ctx, wtPath, task, log); verr == nil {
				verr = verify(ctx, wt, files, log)
			}
			if verr == nil || ctx.Err() != nil {
				break
			}
			fmt.Fprintf(log, "attempt failed: %v\n", verr)
			task.PrevFailure = verr.Error()
		}
		if verr != nil {
			abort(wt)
			if ctx.Err() != nil {
				return Failed, steps, false, ctx.Err()
			}
			return Unresolved, steps, false, fmt.Errorf("%w: %v", ErrUnresolved, verr)
		}
		if err := wt.Run(ctx, append([]string{"add", "-A", "--"}, files...)...); err != nil {
			abort(wt)
			return Failed, steps, false, err
		}
		if err := continueRebase(ctx, wt); err != nil {
			abort(wt)
			return Failed, steps, false, err
		}
	}

	newSHA, err := wt.Output(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Failed, steps, false, err
	}
	if newSHA == origSHA {
		return Skipped, steps, false, nil
	}
	if o.VerifyCmd != "" {
		fmt.Fprintf(log, "\n$ %s\n", o.VerifyCmd)
		cmd := exec.CommandContext(ctx, "sh", "-c", o.VerifyCmd)
		cmd.Dir, cmd.Stdout, cmd.Stderr = wtPath, log, log
		if err := cmd.Run(); err != nil {
			return Failed, steps, false, fmt.Errorf("verify command failed: %w", err)
		}
	}

	// 3.5 Force-push, but only if nobody pushed to the branch meanwhile.
	if o.DryRun {
		fmt.Fprintf(log, "\ndry-run: would push %s -> %s (was %s)\n", newSHA, pr.Head, origSHA)
		return Rebased, steps, false, nil
	}
	if err := repo.Run(ctx, "push",
		fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", pr.Head, origSHA),
		remote, fmt.Sprintf("%s:refs/heads/%s", newSHA, pr.Head)); err != nil {
		return Failed, steps, false, fmt.Errorf("push: %w", err)
	}
	return Rebased, steps, true, nil
}

func (r *Resolver) removeWorktree(repo *gitx.Git, path, branch string) {
	ctx := context.Background() // cleanup must run even if ctx is cancelled
	_ = repo.Run(ctx, "worktree", "remove", "--force", path)
	_ = os.RemoveAll(path)
	_ = repo.Run(ctx, "worktree", "prune")
	_ = repo.Run(ctx, "branch", "-D", branch)
}

func abort(wt *gitx.Git) {
	ctx := context.Background()
	if wt.RebaseInProgress(ctx) {
		_ = wt.Run(ctx, "rebase", "--abort")
	}
}

// continueRebase continues the rebase, skipping steps that became empty.
// Stopping on the next conflicting commit is not an error.
func continueRebase(ctx context.Context, wt *gitx.Git) error {
	args := []string{"-c", "core.editor=true", "rebase", "--continue"}
	if wt.Run(ctx, "diff", "--cached", "--quiet") == nil {
		args = []string{"rebase", "--skip"} // nothing staged: the commit is now empty
	}
	err := wt.Run(ctx, args...)
	if err == nil || !wt.RebaseInProgress(ctx) {
		return err
	}
	if files, _ := wt.Unmerged(ctx); len(files) > 0 {
		return nil // stopped on the next conflict
	}
	return err
}

var markerRe = regexp.MustCompile(`^(<{7}|>{7}|\|{7})( |$)`)

// verify checks that the agent removed every conflict marker in the
// conflicted files and reverts any changes it made to other files.
func verify(ctx context.Context, wt *gitx.Git, files []string, log io.Writer) error {
	var problems []string
	for _, f := range files {
		lines, err := markerLines(filepath.Join(wt.Dir, f))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if len(lines) > 0 {
			problems = append(problems, fmt.Sprintf("%s still has conflict markers on line(s) %s", f, joinInts(lines, 10)))
		}
	}
	if err := revertStray(ctx, wt, files, log); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// markerLines returns the 1-based line numbers holding conflict markers.
// A deleted file is fine (modify/delete conflicts may be resolved by deletion).
func markerLines(path string) ([]int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		if markerRe.Match(sc.Bytes()) {
			out = append(out, n)
		}
	}
	return out, sc.Err()
}

// revertStray undoes worktree changes outside the conflicted files, so a
// wandering agent cannot sneak unrelated edits into the PR.
func revertStray(ctx context.Context, wt *gitx.Git, allowed []string, log io.Writer) error {
	ok := map[string]bool{}
	for _, f := range allowed {
		ok[f] = true
	}
	out, err := wt.Output(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		x, y, path := e[0], e[1], e[3:]
		if x == 'R' || x == 'C' {
			i++ // skip the rename source
		}
		if ok[path] {
			continue
		}
		switch {
		case x == '?' && y == '?':
			fmt.Fprintf(log, "reverting stray new file %s\n", path)
			if err := os.RemoveAll(filepath.Join(wt.Dir, path)); err != nil {
				return err
			}
		case y != ' ':
			fmt.Fprintf(log, "reverting stray change to %s\n", path)
			if err := wt.Run(ctx, "checkout", "--", path); err != nil {
				return err
			}
		}
	}
	return nil
}

func joinInts(ns []int, max int) string {
	var s []string
	for i, n := range ns {
		if i == max {
			s = append(s, "...")
			break
		}
		s = append(s, fmt.Sprint(n))
	}
	return strings.Join(s, ", ")
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

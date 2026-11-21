// Package gitx is a thin wrapper around the git CLI.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Git runs git commands in Dir. Command output is mirrored to Log.
type Git struct {
	Dir string
	Env []string // extra environment, KEY=VALUE
	Log io.Writer
}

// Output runs git and returns trimmed stdout. Stderr goes to the log and is
// included in the error.
func (g *Git) Output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")
	cmd.Env = append(cmd.Env, g.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if g.Log != nil {
		fmt.Fprintf(g.Log, "$ git %s\n", strings.Join(args, " "))
		cmd.Stdout = io.MultiWriter(&stdout, g.Log)
		cmd.Stderr = io.MultiWriter(&stderr, g.Log)
	}
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()+"\n"+stdout.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Run runs git, discarding stdout.
func (g *Git) Run(ctx context.Context, args ...string) error {
	_, err := g.Output(ctx, args...)
	return err
}

// Lines runs git and splits stdout into non-empty lines.
func (g *Git) Lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := g.Output(ctx, args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// GitDir returns the absolute git dir (per-worktree).
func (g *Git) GitDir(ctx context.Context) (string, error) {
	d, err := g.Output(ctx, "rev-parse", "--absolute-git-dir")
	return d, err
}

// RebaseInProgress reports whether a rebase is underway in the worktree.
func (g *Git) RebaseInProgress(ctx context.Context) bool {
	dir, err := g.GitDir(ctx)
	if err != nil {
		return false
	}
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if st, err := os.Stat(filepath.Join(dir, d)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// Unmerged lists paths with unresolved conflicts.
func (g *Git) Unmerged(ctx context.Context) ([]string, error) {
	return g.Lines(ctx, "diff", "--name-only", "--diff-filter=U")
}

// Package config loads runtime configuration from flags and environment
// variables. Every flag falls back to an environment variable, which falls
// back to a sensible default, so the tool works the same locally and in CI.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config holds all settings for a run.
type Config struct {
	Repo         string // owner/name
	Token        string
	APIURL       string
	Remote       string
	RepoDir      string // local clone the worktrees are created from
	Model        string
	OllamaHost   string
	MaxParallel  int
	MaxSteps     int
	AgentTimeout time.Duration
	AgentRetries int
	VerifyCmd    string
	DryRun       bool
	Comment      bool
	SkipDrafts   bool
	SkipLabel    string
	AgentCmd     string
	WorkRoot     string
	Strict       bool
	GitName      string
	GitEmail     string
	SkipPull     bool // don't ensure the model is present in Ollama
	ListOnly     bool // only list conflicting PRs
}

// Load parses args (without the program name) on top of the environment.
func Load(args []string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("merge-conflict-loop", flag.ContinueOnError)

	fs.StringVar(&c.Repo, "repo", env("GITHUB_REPOSITORY", ""), "GitHub repository owner/name (default: inferred from the remote)")
	fs.StringVar(&c.Token, "token", "", "GitHub token (default: env GH_TOKEN or GITHUB_TOKEN)")
	fs.StringVar(&c.APIURL, "api-url", env("GITHUB_API_URL", "https://api.github.com"), "GitHub API base URL")
	fs.StringVar(&c.Remote, "remote", env("GIT_REMOTE", "origin"), "git remote to fetch from and push to")
	fs.StringVar(&c.RepoDir, "repo-dir", env("REPO_DIR", "."), "path to the local clone")
	fs.StringVar(&c.Model, "model", env("LLM_MODEL", "qwen2.5-coder:7b"), "Ollama model used by the agent")
	fs.StringVar(&c.OllamaHost, "ollama-host", ollamaHost(), "Ollama base URL")
	fs.IntVar(&c.MaxParallel, "max-parallel", envInt("MAX_PARALLEL", 2), "PRs resolved concurrently")
	fs.IntVar(&c.MaxSteps, "max-steps", envInt("MAX_STEPS", 50), "max rebase steps per PR")
	fs.DurationVar(&c.AgentTimeout, "agent-timeout", envDuration("AGENT_TIMEOUT", 15*time.Minute), "timeout for a single agent invocation")
	fs.IntVar(&c.AgentRetries, "agent-retries", envInt("AGENT_RETRIES", 1), "retries per conflicting rebase step")
	fs.StringVar(&c.VerifyCmd, "verify-cmd", env("VERIFY_CMD", ""), "optional shell command that must pass before pushing")
	fs.BoolVar(&c.DryRun, "dry-run", envBool("DRY_RUN", false), "resolve but do not push")
	fs.BoolVar(&c.Comment, "comment", envBool("COMMENT", false), "comment on PRs with the outcome")
	fs.BoolVar(&c.SkipDrafts, "skip-drafts", envBool("SKIP_DRAFTS", false), "ignore draft PRs")
	fs.StringVar(&c.SkipLabel, "skip-label", env("SKIP_LABEL", "no-auto-rebase"), "PRs with this label are ignored")
	fs.StringVar(&c.AgentCmd, "agent-cmd", env("AGENT_CMD", "pi"), "agent executable (pi-compatible CLI)")
	fs.StringVar(&c.WorkRoot, "work-root", env("WORK_ROOT", defaultWorkRoot()), "directory for worktrees and logs")
	fs.BoolVar(&c.Strict, "strict", envBool("STRICT", false), "exit 2 if any PR could not be resolved")
	fs.StringVar(&c.GitName, "git-name", env("GIT_COMMITTER_NAME", "merge-conflict-loop[bot]"), "committer name")
	fs.StringVar(&c.GitEmail, "git-email", env("GIT_COMMITTER_EMAIL", "merge-conflict-loop@users.noreply.github.com"), "committer email")
	fs.BoolVar(&c.SkipPull, "skip-model-check", envBool("SKIP_MODEL_CHECK", false), "don't check/pull the model in Ollama")

	fs.BoolVar(&c.ListOnly, "list-only", false, "only list conflicting PRs and exit")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.Repo == "" {
		c.Repo = inferRepo(c.RepoDir, c.Remote)
	}
	if c.Repo == "" || !strings.Contains(c.Repo, "/") {
		return nil, errors.New("cannot determine repository: set --repo or GITHUB_REPOSITORY")
	}
	if c.Token == "" { // not a flag default, so -h never prints a secret
		c.Token = env("GH_TOKEN", env("GITHUB_TOKEN", ""))
	}
	if c.Token == "" {
		return nil, errors.New("missing GitHub token: set GH_TOKEN or GITHUB_TOKEN")
	}
	if c.MaxParallel < 1 {
		c.MaxParallel = 1
	}
	if c.MaxSteps < 1 {
		return nil, fmt.Errorf("max-steps must be >= 1")
	}
	c.APIURL = strings.TrimRight(c.APIURL, "/")
	c.OllamaHost = strings.TrimRight(c.OllamaHost, "/")
	return c, nil
}

func ollamaHost() string {
	h := env("OLLAMA_HOST", "http://localhost:11434")
	if !strings.Contains(h, "://") { // ollama accepts bare host:port
		h = "http://" + h
	}
	return h
}

func defaultWorkRoot() string {
	if d := os.Getenv("RUNNER_TEMP"); d != "" {
		return d + "/merge-conflict-loop"
	}
	return os.TempDir() + "/merge-conflict-loop"
}

var remoteRe = regexp.MustCompile(`github\.com[:/]([^/]+/[^/]+?)(?:\.git)?/?$`)

func inferRepo(dir, remote string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", remote).Output()
	if err != nil {
		return ""
	}
	if m := remoteRe.FindStringSubmatch(strings.TrimSpace(string(out))); m != nil {
		return m[1]
	}
	return ""
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

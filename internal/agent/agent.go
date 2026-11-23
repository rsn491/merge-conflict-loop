// Package agent runs a coding agent (pi by default) to resolve conflicts,
// backed by a local open-weight model served by Ollama.
package agent

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed prompt.md
var SystemPrompt string

// Task describes one conflicting rebase step.
type Task struct {
	PRNumber    int
	PRTitle     string
	Base        string
	Head        string
	Commit      string // subject/description of the commit being replayed
	Files       []string
	PrevFailure string // set on retries
}

// Agent resolves conflicts in dir. It must leave the files conflict-free;
// the caller verifies that independently.
type Agent interface {
	Resolve(ctx context.Context, dir string, t Task, log io.Writer) error
}

// Pi drives the pi coding agent in print (headless) mode.
type Pi struct {
	Cmd        string // executable, default "pi"
	Model      string // Ollama model id, e.g. qwen2.5-coder:7b
	OllamaHost string // e.g. http://localhost:11434
	ConfigDir  string // private PI_CODING_AGENT_DIR
	Timeout    time.Duration
}

// Setup writes the private pi config (models.json + system prompt file).
func (p *Pi) Setup() error {
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		return err
	}
	cfg := map[string]any{
		"providers": map[string]any{
			"ollama": map[string]any{
				"baseUrl": p.OllamaHost + "/v1",
				"api":     "openai-completions",
				"apiKey":  "ollama",
				"models":  []map[string]any{{"id": p.Model}},
			},
		},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(p.ConfigDir, "models.json"), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(p.promptPath(), []byte(SystemPrompt), 0o644)
}

func (p *Pi) promptPath() string { return filepath.Join(p.ConfigDir, "merge-conflict-prompt.md") }

// BuildPrompt renders the user prompt for a task.
func BuildPrompt(t Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pull request #%d %q is being rebased from %q onto %q.\n", t.PRNumber, t.PRTitle, t.Head, t.Base)
	fmt.Fprintf(&b, "Replaying commit:\n%s\n\n", indent(t.Commit))
	b.WriteString("These files contain merge conflicts that you must resolve:\n")
	for _, f := range t.Files {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	if t.PrevFailure != "" {
		fmt.Fprintf(&b, "\nA previous attempt failed verification:\n%s\nFix this.\n", indent(t.PrevFailure))
	}
	b.WriteString("\nResolve every conflict now, following the rules in the system prompt.")
	return b.String()
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n    ")
}

func (p *Pi) Resolve(ctx context.Context, dir string, t Task, log io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	args := []string{
		"--print",
		"--no-session",
		"--model", "ollama/" + p.Model,
		"--thinking", "off",
		"--tools", "read,edit,write,bash,grep,find,ls",
		"--append-system-prompt", p.promptPath(),
		BuildPrompt(t),
	}
	cmd := exec.CommandContext(ctx, p.Cmd, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PI_CODING_AGENT_DIR="+p.ConfigDir,
		"MCL_PR_NUMBER="+fmt.Sprint(t.PRNumber),
		"MCL_CONFLICTED_FILES="+strings.Join(t.Files, "\n"),
	)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = 10 * time.Second
	fmt.Fprintf(log, "$ %s [agent, %d file(s)]\n", p.Cmd, len(t.Files))
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("agent timed out after %s", p.Timeout)
		}
		return fmt.Errorf("agent failed: %w", err)
	}
	return nil
}

// EnsureModel makes sure Ollama is reachable and has the model, pulling it
// if necessary.
func EnsureModel(ctx context.Context, host, model string, log io.Writer) error {
	hc := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, host+"/api/tags", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("ollama not reachable at %s (is `ollama serve` running?): %w", host, err)
	}
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tags)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("ollama /api/tags: %w", err)
	}
	for _, m := range tags.Models {
		if m.Name == model || m.Model == model || m.Name == model+":latest" {
			return nil
		}
	}
	fmt.Fprintf(log, "pulling model %s from ollama registry...\n", model)
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/pull", bytes.NewReader(body))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ollama pull: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ollama pull: %s: %s", resp.Status, msg)
	}
	sc := bufio.NewScanner(resp.Body)
	last := ""
	for sc.Scan() {
		var ev struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Error != "" {
			return fmt.Errorf("ollama pull: %s", ev.Error)
		}
		if ev.Status != last { // skip per-chunk progress noise
			fmt.Fprintf(log, "  %s\n", ev.Status)
			last = ev.Status
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("ollama pull: %w", err)
	}
	if last != "success" {
		return fmt.Errorf("ollama pull did not complete (last status %q)", last)
	}
	return nil
}

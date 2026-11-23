package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPiInvocation(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-pi")
	// Records its args and env so we can assert on the pi command line.
	os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > args.txt\necho \"$PI_CODING_AGENT_DIR\" > env.txt\n"), 0o755)
	p := &Pi{Cmd: script, Model: "qwen2.5-coder:7b", OllamaHost: "http://h:1", ConfigDir: filepath.Join(dir, "cfg"), Timeout: 5 * time.Second}
	if err := p.Setup(); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]map[string]map[string]any
	b, _ := os.ReadFile(filepath.Join(p.ConfigDir, "models.json"))
	if err := json.Unmarshal(b, &cfg); err != nil || cfg["providers"]["ollama"]["baseUrl"] != "http://h:1/v1" {
		t.Fatalf("models.json = %s (%v)", b, err)
	}
	work := t.TempDir()
	var log bytes.Buffer
	err := p.Resolve(context.Background(), work, Task{PRNumber: 3, Files: []string{"a.go"}, Commit: "abc msg"}, &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	args, _ := os.ReadFile(filepath.Join(work, "args.txt"))
	for _, want := range []string{"--print", "--no-session", "ollama/qwen2.5-coder:7b", "- a.go"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	env, _ := os.ReadFile(filepath.Join(work, "env.txt"))
	if strings.TrimSpace(string(env)) != p.ConfigDir {
		t.Errorf("PI_CODING_AGENT_DIR = %q", env)
	}
}

func TestPiTimeout(t *testing.T) {
	script := filepath.Join(t.TempDir(), "slow")
	os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 10\n"), 0o755)
	p := &Pi{Cmd: script, ConfigDir: t.TempDir(), Timeout: 200 * time.Millisecond}
	err := p.Resolve(context.Background(), t.TempDir(), Task{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureModel(t *testing.T) {
	pulled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"present:latest","model":"present:latest"}]}`))
		case "/api/pull":
			pulled = true
			w.Write([]byte("{\"status\":\"pulling manifest\"}\n{\"status\":\"success\"}\n"))
		}
	}))
	defer srv.Close()
	var log bytes.Buffer
	if err := EnsureModel(context.Background(), srv.URL, "present", &log); err != nil || pulled {
		t.Fatalf("present model: err=%v pulled=%v", err, pulled)
	}
	if err := EnsureModel(context.Background(), srv.URL, "missing:7b", &log); err != nil || !pulled {
		t.Fatalf("missing model: err=%v pulled=%v", err, pulled)
	}
	if err := EnsureModel(context.Background(), "http://127.0.0.1:1", "x", &log); err == nil {
		t.Fatal("expected unreachable error")
	}
}

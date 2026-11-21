package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestListOpenPRsPaginates(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing auth header")
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls?state=open&per_page=100&page=2>; rel="next"`, srv.URL))
			json.NewEncoder(w).Encode([]PR{{Number: 1}, {Number: 2}})
			return
		}
		json.NewEncoder(w).Encode([]PR{{Number: 3}})
	}))
	defer srv.Close()

	prs, err := New(srv.URL, "o/r", "tok").ListOpenPRs(context.Background())
	if err != nil || len(prs) != 3 || prs[2].Number != 3 {
		t.Fatalf("got %v, %v", prs, err)
	}
}

func TestResolveMergeabilityPolls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Write([]byte(`{"number":5,"mergeable":null,"mergeable_state":"unknown"}`))
			return
		}
		w.Write([]byte(`{"number":5,"mergeable":false,"mergeable_state":"dirty"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "tok")
	c.PollDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	pr, err := c.ResolveMergeability(context.Background(), 5)
	if err != nil || !IsConflicting(pr) || calls.Load() != 3 {
		t.Fatalf("pr=%+v err=%v calls=%d", pr, err, calls.Load())
	}
}

func TestPRHelpers(t *testing.T) {
	var pr PR
	json.Unmarshal([]byte(`{"labels":[{"name":"No-Auto-Rebase"}],
		"head":{"ref":"f","repo":{"full_name":"x/r"}},"base":{"ref":"main","repo":{"full_name":"o/r"}},
		"mergeable":true,"mergeable_state":"clean"}`), &pr)
	if !pr.IsFork() || !pr.HasLabel("no-auto-rebase") || IsConflicting(pr) {
		t.Fatalf("%+v", pr)
	}
	pr.Head.Repo.FullName = "o/r"
	if pr.IsFork() {
		t.Fatal("same repo reported as fork")
	}
}

func TestErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "o/r", "x").ListOpenPRs(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

// Package gh is a minimal GitHub REST client covering what the tool needs:
// listing open PRs, resolving their (lazily computed) mergeability, and
// posting comments.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// PR is the subset of a pull request the tool uses.
type PR struct {
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	Draft          bool    `json:"draft"`
	Mergeable      *bool   `json:"mergeable"`
	MergeableState string  `json:"mergeable_state"`
	Labels         []Label `json:"labels"`
	Head           Ref     `json:"head"`
	Base           Ref     `json:"base"`
}

type Label struct {
	Name string `json:"name"`
}

type Ref struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

// IsFork reports whether the PR head lives in a different repository.
func (p PR) IsFork() bool {
	return p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.FullName != p.Base.Repo.FullName
}

// HasLabel reports whether the PR carries the given label.
func (p PR) HasLabel(name string) bool {
	for _, l := range p.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// Client talks to the GitHub REST API.
type Client struct {
	BaseURL string
	Repo    string
	Token   string
	HTTP    *http.Client
	// PollDelays are the waits between mergeability polls.
	PollDelays []time.Duration
}

func New(baseURL, repo, token string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Repo:       repo,
		Token:      token,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
		PollDelays: []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second, 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (http.Header, error) {
	url := path
	if !strings.HasPrefix(path, "http") {
		url = c.BaseURL + path
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "merge-conflict-loop")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return resp.Header, nil
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// ListOpenPRs returns all open PRs (all pages).
func (c *Client) ListOpenPRs(ctx context.Context) ([]PR, error) {
	var all []PR
	next := fmt.Sprintf("/repos/%s/pulls?state=open&per_page=100", c.Repo)
	for next != "" {
		var page []PR
		h, err := c.do(ctx, http.MethodGet, next, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = ""
		if m := nextLinkRe.FindStringSubmatch(h.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return all, nil
}

// GetPR fetches a single PR, which also triggers GitHub's background
// mergeability computation.
func (c *Client) GetPR(ctx context.Context, n int) (PR, error) {
	var pr PR
	_, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, n), nil, &pr)
	return pr, err
}

// ResolveMergeability polls a PR until GitHub has computed whether it is
// mergeable. It returns the last fetched PR; Mergeable may still be nil if
// GitHub never finished computing it.
func (c *Client) ResolveMergeability(ctx context.Context, n int) (PR, error) {
	pr, err := c.GetPR(ctx, n)
	for i := 0; err == nil && !known(pr) && i < len(c.PollDelays); i++ {
		select {
		case <-ctx.Done():
			return pr, ctx.Err()
		case <-time.After(c.PollDelays[i]):
		}
		pr, err = c.GetPR(ctx, n)
	}
	return pr, err
}

func known(p PR) bool {
	return p.Mergeable != nil && p.MergeableState != "" && p.MergeableState != "unknown"
}

// IsConflicting reports whether GitHub says the PR has merge conflicts.
func IsConflicting(p PR) bool {
	return p.Mergeable != nil && !*p.Mergeable && p.MergeableState == "dirty"
}

// Comment posts an issue comment on a PR.
func (c *Client) Comment(ctx context.Context, n int, body string) error {
	_, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", c.Repo, n), map[string]string{"body": body}, nil)
	return err
}

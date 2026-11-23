package config

import "testing"

func TestCommitterDefaultsToActionsBot(t *testing.T) {
	t.Setenv("GH_TOKEN", "x")
	t.Setenv("GIT_COMMITTER_NAME", "")
	t.Setenv("GIT_COMMITTER_EMAIL", "")
	c, err := Load([]string{"--repo", "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	if c.GitName != "github-actions[bot]" || c.GitEmail != "41898282+github-actions[bot]@users.noreply.github.com" {
		t.Fatalf("got %q <%s>", c.GitName, c.GitEmail)
	}
}

func TestCommitterOverride(t *testing.T) {
	t.Setenv("GH_TOKEN", "x")
	t.Setenv("GIT_COMMITTER_NAME", "my-bot")
	t.Setenv("GIT_COMMITTER_EMAIL", "bot@example.com")
	c, err := Load([]string{"--repo", "o/r"})
	if err != nil || c.GitName != "my-bot" || c.GitEmail != "bot@example.com" {
		t.Fatalf("env override: %+v %v", c, err)
	}
	c, err = Load([]string{"--repo", "o/r", "--git-name", "flag-bot", "--git-email", "f@example.com"})
	if err != nil || c.GitName != "flag-bot" || c.GitEmail != "f@example.com" {
		t.Fatalf("flag override: %+v %v", c, err)
	}
}

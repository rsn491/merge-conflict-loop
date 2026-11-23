# merge-conflict-loop

When a push lands on `main`, this tool finds the open pull requests that now have merge conflicts and rebases each one onto its base branch. A coding agent ([pi](https://pi.dev)) resolves the conflicts, and the branch is force-pushed back.

By default the agent uses a small **open-weight model, `qwen2.5-coder:7b`, served locally by [Ollama](https://ollama.com)**. There are no API keys and no per-token costs, and it runs on a standard GitHub runner or a laptop with no GPU.

## How it works

```
push to main
  └─ list open PRs ──► poll mergeability ──► keep "dirty" (conflicting) PRs
        └─ for each PR (goroutine, at most MAX_PARALLEL at once)
             1. git worktree add <tmp>/pr-N origin/<head>   (isolated workspace)
             2. git rebase origin/<base>
             3. for every conflicting rebase step:
                  pi --print … "resolve the conflicts in <files>"
                  verify: no conflict markers left, revert stray edits
                  (retry with the failure reason, then give up)
                  git add + git rebase --continue
             4. optional VERIFY_CMD (e.g. `go test ./...`)
             5. git push --force-with-lease=<head>:<original sha>
```

Safety properties:
- **Nothing is pushed unless every conflict was resolved.** The tool checks the agent's output itself instead of trusting the model. If anything fails, the rebase is aborted and the PR is left untouched.
- **`--force-with-lease`.** If the author pushes to the branch while the agent is working, the push is rejected and their work is not overwritten.
- **The agent may edit only the conflicted files.** Any other edit it makes is reverted. Original commit authors are kept, and the committer is set to `github-actions[bot]` (see [Bot identity](#bot-identity)).
- **Some PRs are skipped:** PRs from forks (the token can't push to them), PRs with the `no-auto-rebase` label, and drafts when `SKIP_DRAFTS=true`.

## GitHub Action

[`.github/workflows/resolve-conflicts.yml`](.github/workflows/resolve-conflicts.yml) runs on every push to `main`, and you can start it manually with `workflow_dispatch` (dry run by default).

1. It builds the binary and runs a quick `--list-only` check. If no PR conflicts, the job stops there.
2. Otherwise it installs pi and Ollama. The model is cached with `actions/cache`, so the roughly 4.7 GB download happens only once.
3. It resolves the PRs, writes a summary table to the job summary, and uploads the per-PR logs as an artifact.

You can set these repository **variables** (Settings → Variables) to change its behaviour: `LLM_MODEL`, `MAX_PARALLEL`, `AGENT_TIMEOUT`, `VERIFY_CMD`, `COMMENT`, `SKIP_DRAFTS`.

### Bot identity

By default everything appears as GitHub's built-in Actions bot:
- **Comments** are posted with the workflow's `GITHUB_TOKEN`, so they come from `github-actions[bot]`.
- **Rebased commits** use the committer `github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>`. This is the identity GitHub recommends in the [`actions/checkout` README](https://github.com/actions/checkout#push-a-commit-using-the-built-in-token). The email has the form `{user.id}+{user.login}@users.noreply.github.com`, which links the commits to the bot's account. Original authors are kept, so GitHub shows "<author> authored and github-actions[bot] committed".

To use a different identity, set the repository variables `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL`, or pass `--git-name` and `--git-email` when running locally. On GitHub Enterprise Server the bot has a different ID, so look it up at `https://<host>/api/v3/users/github-actions%5Bbot%5D` and set `GIT_COMMITTER_EMAIL` to match.

> **CI on rebased PRs:** GitHub does not start workflows for pushes made with the default `GITHUB_TOKEN`. To have the PR's checks re-run after a rebase, add a `REBASE_TOKEN` secret: a GitHub App token or fine-grained PAT with `contents: write`. It is used **only for the push**. Comments still come from `github-actions[bot]`, but the PR timeline will show the token's owner as the one who force-pushed. Pushes to PR branches never re-trigger this workflow, because it only listens to `main`.

## Running locally

```sh
ollama serve &                                   # any machine, CPU is fine
npm i -g --ignore-scripts @earendil-works/pi-coding-agent
export GH_TOKEN=$(gh auth token)
go run ./cmd/merge-conflict-loop --dry-run       # from a clone of the repo
```

## Configuration

Every flag also has an environment variable:

| Flag | Env | Default | |
|---|---|---|---|
| `--repo` | `GITHUB_REPOSITORY` | from `origin` URL | `owner/name` |
| `--token` | `GH_TOKEN` / `GITHUB_TOKEN` | — | required |
| `--model` | `LLM_MODEL` | `qwen2.5-coder:7b` | any Ollama model with tool calling |
| `--ollama-host` | `OLLAMA_HOST` | `http://localhost:11434` | any OpenAI-compatible server |
| `--max-parallel` | `MAX_PARALLEL` | `2` | PRs resolved concurrently |
| `--max-steps` | `MAX_STEPS` | `50` | max conflicting commits per PR |
| `--agent-timeout` | `AGENT_TIMEOUT` | `15m` | per agent call |
| `--agent-retries` | `AGENT_RETRIES` | `1` | retries per step, with the failure reason fed back |
| `--verify-cmd` | `VERIFY_CMD` | — | must pass before pushing |
| `--dry-run` | `DRY_RUN` | `false` | resolve but don't push |
| `--comment` | `COMMENT` | `false` | comment on each PR with the outcome |
| `--skip-drafts` | `SKIP_DRAFTS` | `false` | |
| `--skip-label` | `SKIP_LABEL` | `no-auto-rebase` | opt-out label |
| `--agent-cmd` | `AGENT_CMD` | `pi` | any pi-compatible CLI |
| `--work-root` | `WORK_ROOT` | `$RUNNER_TEMP` or `/tmp` | worktrees and logs |
| `--git-name` | `GIT_COMMITTER_NAME` | `github-actions[bot]` | committer of rebased commits |
| `--git-email` | `GIT_COMMITTER_EMAIL` | `41898282+github-actions[bot]@…` | committer email |
| `--strict` | `STRICT` | `false` | exit 2 if any PR was not resolved |
| `--list-only` | — | `false` | print conflicting PRs and exit |

**Exit codes:**
- `0`: the run finished. PRs that could not be resolved are reported, not treated as fatal.
- `1`: an infrastructure error (GitHub API, git, Ollama).
- `2`: `--strict` is set and at least one PR was not resolved.

**Choosing a model:**
- Bigger models resolve conflicts more reliably but run slower on CPU. `qwen2.5-coder:14b` fits a 16 GB runner.
- The tool writes a private pi config in `<work-root>/pi-agent`, so your own `~/.pi` is never touched.
- To use a hosted open-weight endpoint instead, point `OLLAMA_HOST` at any OpenAI-compatible `/v1` server.

## Development

```sh
go test -race ./...
```

The resolver tests build real git repos (a bare `origin`, conflicting branches) and use a scripted fake agent. They cover these cases:
- a successful rebase and push;
- a dry run;
- an unresolved conflict with a retry;
- stray-edit revert;
- force-with-lease protection;
- `VERIFY_CMD`;
- concurrent PRs.

## Limitations

- A 7B model can fail on large or semantic conflicts. When that happens the PR is left as it was, and with `COMMENT=true` the PR gets a comment asking for a manual rebase.
- Only conflicts that git reports are resolved. If the rebased code compiles but is logically wrong, only `VERIFY_CMD` can catch it.
- PRs from forks are not supported, because the token has no push access to them.

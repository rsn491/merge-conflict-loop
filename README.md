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

## Quick start (GitHub Action)

Add [`examples/resolve-conflicts.yml`](examples/resolve-conflicts.yml) to your repository as `.github/workflows/resolve-conflicts.yml`:

```yaml
name: Resolve PR merge conflicts
on:
  push:
    branches: [main]
  workflow_dispatch:
    inputs:
      dry_run: { type: boolean, default: true }
permissions:
  contents: write
  pull-requests: write
jobs:
  resolve:
    runs-on: ubuntu-latest
    timeout-minutes: 90
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0                           # required: the tool rebases locally
      - uses: rsn491/merge-conflict-loop@v0.0.1
        with:
          dry-run: ${{ github.event_name == 'workflow_dispatch' && inputs.dry_run }}
```

That's all: no secrets and no API keys. Start it once from the Actions tab (a dry run by default) to see what it would do.

On each push to `main` the action:
1. installs the CLI (the release matching the action's tag, through [`install.sh`](install.sh));
2. runs a quick `--list-only` check, and stops there if no PR conflicts;
3. installs pi and Ollama and pulls the model, which is cached with `actions/cache` so the roughly 4.7 GB download happens once;
4. resolves the PRs, writes a summary table to the job summary, and uploads the per-PR logs as an artifact.

### Action inputs

| Input | Default | |
|---|---|---|
| `model` | `qwen2.5-coder:7b` | any Ollama model with tool calling |
| `max-parallel` | `2` | PRs resolved concurrently |
| `agent-timeout` | `15m` | per agent call |
| `verify-cmd` | — | e.g. `npm test`; must pass before pushing |
| `comment` | `false` | comment on each PR with the outcome |
| `skip-drafts` | `false` | ignore draft PRs |
| `skip-label` | `no-auto-rebase` | PRs with this label are ignored |
| `dry-run` | `false` | resolve but don't push |
| `committer-name` | `github-actions[bot]` | committer of rebased commits |
| `committer-email` | `41898282+github-actions[bot]@users.noreply.github.com` | |
| `github-token` | `${{ github.token }}` | API calls (list PRs, comments) |
| `version` | the action's tag | CLI release to install; from an untagged ref (`@main`, `./`) the CLI is built from source |

Output: `conflicting`, the number of conflicting PRs found.

**Pushes use the credentials of your `actions/checkout` step.** To use another token for pushing, pass it there (`token: ${{ secrets.REBASE_TOKEN }}`). See the CI note below.

### Bot identity

By default everything appears as GitHub's built-in Actions bot:
- **Comments** are posted with the workflow's `GITHUB_TOKEN`, so they come from `github-actions[bot]`.
- **Rebased commits** use the committer `github-actions[bot] <41898282+github-actions[bot]@users.noreply.github.com>`. This is the identity GitHub recommends in the [`actions/checkout` README](https://github.com/actions/checkout#push-a-commit-using-the-built-in-token). The email has the form `{user.id}+{user.login}@users.noreply.github.com`, which links the commits to the bot's account. Original authors are kept, so GitHub shows "<author> authored and github-actions[bot] committed".

To use a different identity, set the `committer-name` and `committer-email` inputs, or pass `--git-name` and `--git-email` to the CLI. On GitHub Enterprise Server the bot has a different ID, so look it up at `https://<host>/api/v3/users/github-actions%5Bbot%5D` and set `committer-email` to match.

> **CI on rebased PRs:** GitHub does not start workflows for pushes made with the default `GITHUB_TOKEN`. To have the PR's checks re-run after a rebase, add a `REBASE_TOKEN` secret (a GitHub App token or fine-grained PAT with `contents: write`) and pass it as `token:` to `actions/checkout`. It is used **only for the push**. Comments still come from `github-actions[bot]`, but the PR timeline will show the token's owner as the one who force-pushed. Pushes to PR branches never re-trigger this workflow, because it only listens to `main`.

## Install the CLI

```sh
curl -fsSL https://raw.githubusercontent.com/rsn491/merge-conflict-loop/main/install.sh | sh
# a specific release:
curl -fsSL https://raw.githubusercontent.com/rsn491/merge-conflict-loop/main/install.sh | VERSION=v0.0.1 sh
```

The installer downloads a prebuilt binary (Linux or macOS, amd64 or arm64) from GitHub Releases and verifies its SHA-256 checksum. It installs to `/usr/local/bin` if that is writable, else to `~/.local/bin`; set `INSTALL_DIR` to choose another location. You can also run `go install github.com/rsn491/merge-conflict-loop/cmd/merge-conflict-loop@latest`.

Run it from a clone of the repository whose PRs you want to fix:

```sh
npm i -g --ignore-scripts @earendil-works/pi-coding-agent   # the agent (Node 22+)
ollama serve &                                              # CPU is fine
cd your-repo
GH_TOKEN=$(gh auth token) merge-conflict-loop --dry-run
```

## CLI configuration

Every flag also has an environment variable (the action sets these from its inputs):

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
| `--version` | — | | print the version and exit |

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

## Releasing

1. Merge to `main`, then tag and push: `git tag v0.0.1 && git push origin v0.0.1`.
2. [`release.yml`](.github/workflows/release.yml) runs the tests, cross-compiles with [`scripts/build-release.sh`](scripts/build-release.sh), and publishes a GitHub release with `merge-conflict-loop_<os>_<arch>.tar.gz` archives and `checksums.txt`.
3. Test it with the curl installer (`VERSION=v0.0.1`) and with `uses: rsn491/merge-conflict-loop@v0.0.1` in a test repository.
4. To publish to the GitHub Marketplace, edit the release, tick "Publish this Action to the GitHub Marketplace" (it reads `action.yml`'s name, description and branding), and optionally move a major tag for users: `git tag -f v0 && git push -f origin v0`.

## Limitations

- A 7B model can fail on large or semantic conflicts. When that happens the PR is left as it was, and with `COMMENT=true` the PR gets a comment asking for a manual rebase.
- Only conflicts that git reports are resolved. If the rebased code compiles but is logically wrong, only `VERIFY_CMD` can catch it.
- PRs from forks are not supported, because the token has no push access to them.

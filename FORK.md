# About this repository

`loadinglucian/cliproxy` is a standalone copy of [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), the OpenAI/Anthropic-compatible proxy. It is not a GitHub fork on purpose, so no pull request here can target upstream. It adds request logging to a local ClickHouse database and builds its own releases. Changes stay in this repository: never open pull requests, issues, or comments upstream. Reading upstream (fetching tags, the releases API) is fine.

## New files only

Upstream releases land almost daily, and every one is merged into `main`. To keep those merges conflict-free, this repository only adds new files and never edits upstream files (including `README.md`, `.gitignore`, `go.mod`, and upstream workflows).

| Path | What it holds |
| --- | --- |
| `FORK.md` | This document |
| `mise.toml`, `mise.lock` | Go toolchain pin, matching the `go` directive in `go.mod` |
| `scripts/fork/` | `build.sh` (build, test, plugins), `sync.sh` (upstream merge), `disable-foreign-workflows.sh` |
| `.github/workflows/fork-*.yml` | Every workflow this repository runs |
| `plugins/<name>/` | Dynamic-library plugins built with upstream's plugin ABI, for example `plugins/cliproxy-log/` |
| `deploy/clickhouse/` | ClickHouse deployment for the request log |

Upstream ignores `plugins/*` in its `.gitignore`, so add new files under `plugins/` with `git add -f`.

Upstream workflows stay in `.github/workflows/` byte-for-byte but are disabled through the GitHub API. `fork-sync` and every push to `main` (`fork-ci`) disable any active workflow whose file name does not start with `fork-`. Never re-enable or edit them. A workflow that upstream adds can run on the sync PR and the merge push once before it is disabled.

## Build and test locally

Run Go only through mise so the toolchain matches CI:

```sh
mise install
mise exec -- scripts/fork/build.sh all    # server (CGO), go test ./..., plugins
```

CI pins mise 2026.10.18, which only reads `mise.lock` format version 3; newer mise writes version 4. When changing the Go version, regenerate the lock with that mise release (`mise lock --platform linux-x64,macos-arm64`, the platforms `mise.lock` covers) or raise the pin in every `fork-*.yml` first.

`build.sh plugins` builds every `plugins/<name>/` as a c-shared library into `bin/plugins/`. A plugin with its own `go.mod` also gets its own `go test ./...`.

## CI

`fork-ci` runs on every PR to `main` and every push to `main`. Its jobs `build-and-test` and `plugins` are required checks in the `main` ruleset, which also requires a pull request for every change. Merge commits and squash merges are allowed; rebase merges are off.

`build-and-test` runs the full upstream `go test ./...` (about 2 minutes on a runner) and retries failing packages once, because a few upstream tests, such as the Codex token refresh tests in `internal/runtime/executor`, fail intermittently on GitHub's runners. A package that fails twice fails the job.

## Upstream sync

`fork-sync` runs every 6 hours and on demand (Actions, fork-sync, Run workflow; optional upstream tag, optional dry run). It runs `scripts/fork/sync.sh`, which:

1. Reads upstream's latest release tag and stops if that commit is already in `main`.
2. Creates `sync/upstream-<tag>` from `main` and runs `git merge --no-ff <tag>` with the message `chore: merge upstream <tag>`, committed as Lucian.
3. Runs the same build and test commands as CI.
4. Pushes the branch, opens the PR `chore: merge upstream <tag>`, and enables auto-merge with a merge commit. CI runs on the PR and GitHub merges it when the checks pass. If the PR is already open, it only turns auto-merge back on when it is off. PRs from forks never count as sync PRs, whatever their branch name.

Writes use the `SYNC_TOKEN` repository secret, a fine-grained token owned by Lucian for this repository only (Contents, Pull requests, Issues, and Workflows read/write). `GITHUB_TOKEN` cannot push commits that touch `.github/workflows/`, and PRs it opens do not trigger CI. If the secret is missing or expired the run fails with that message; create a new token with the same permissions and save it as `SYNC_TOKEN`.

Steps 1 to 3 run upstream code nobody has reviewed, so the workflow splits them from the writes. The `check` job (`sync.sh --phase check`) merges, builds, and tests with no secrets and a read-only `GITHUB_TOKEN`, and uploads its outcome. The `publish` job, on a separate runner, holds `SYNC_TOKEN`, redoes the merge with git only, and pushes or files the issue only when the check outcome names the same upstream commit, base commit, and merged tree. Locally, `sync.sh` runs every step in one process.

Upstream tags are fetched into `refs/fork-sync/*` on the runner only and are never pushed here. This repository's own tags are the `v*-lucian.*` release tags.

### Notifications

When a merge conflicts or the build or tests fail, nothing is pushed. The run:

- opens, or updates if already open, an issue titled `Upstream <tag> merge conflict` or `Upstream <tag> build failure`, assigned to `loadinglucian`, listing the conflicting files or the failing step and how to resolve it;
- writes the same details to the run's summary page;
- ends red, so GitHub's failed-workflow email reaches Lucian.

Every run also ends red while any open sync PR has failing checks, so a stuck PR keeps sending email until it is fixed or closed. When closing one instead of fixing it, also delete its branch: a later run reuses an existing `sync/upstream-<tag>` branch and would open a new auto-merging PR from it.

To preview a sync without writing anything: `mise exec -- scripts/fork/sync.sh --dry-run [--tag <tag>]`.

### Resolving a sync conflict

Work in a clone of this repository:

```sh
tag=v8.0.61   # the tag named in the issue
git fetch origin main
git fetch --no-tags https://github.com/router-for-me/CLIProxyAPI.git "refs/tags/$tag:refs/fork-sync/$tag"
git switch -c "sync/upstream-$tag" origin/main
git merge --no-ff -m "chore: merge upstream $tag" "refs/fork-sync/$tag"
```

1. Resolve each conflict. Upstream files should end up as upstream has them; move any change this repository needs into its own new files instead. Then `git commit --no-edit`.
2. Run `mise exec -- scripts/fork/build.sh all` and fix failures in this repository's files.
3. Push the branch and open a PR titled `chore: merge upstream <tag>`.
4. Merge it with a merge commit: `gh pr merge <number> --merge`. Never squash or rebase a sync PR. Squashing drops the upstream parent, so the next sync would replay and conflict on every upstream change since.
5. Never push `refs/fork-sync/*` or upstream tags. Close the issue once the PR merges.

A build failure is fixed the same way, except the merge itself succeeds; commit the fix on the sync branch.

## Releases

Release tags are `v<upstream-version>-lucian.<n>`: the upstream release in `main` plus a counter that restarts at 1 for each upstream version.

```sh
git switch main && git pull
git tag -a v8.0.60-lucian.1 -m "v8.0.60-lucian.1"
git push origin v8.0.60-lucian.1
```

`fork-release` refuses a tag whose commit is not on `main`, or whose upstream version is not an ancestor of the tagged commit. It builds on Ubuntu 24.04 (glibc 2.39, fine for Arch) and publishes a release with:

- `CLIProxyAPI_<version>_linux_amd64.tar.gz`: the server (`cli-proxy-api`, CGO, plugin support) with `LICENSE`, `README.md`, `FORK.md`, and `config.example.yaml`;
- `<plugin>_linux_amd64.so` for each `plugins/<name>/`;
- `checksums.txt` with SHA-256 sums.

The binary reports the version without the leading `v`, for example `8.0.60-lucian.1`. To rebuild a tag, delete its release and run fork-release from the Actions tab with that tag.

## Deploying

Deploying to ThinkPad, the Linux laptop that runs the proxy, is manual and never automated from this repository. The procedure lives in Lucian's FLEET skill documentation.

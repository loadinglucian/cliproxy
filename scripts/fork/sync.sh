#!/usr/bin/env bash
# Merges the latest upstream CLIProxyAPI release into this repo's main branch through
# a pull request. fork-sync.yml runs it every 6 hours; it also runs locally.
#
# Usage: scripts/fork/sync.sh [--tag <tag>] [--dry-run] [--repo <owner/name>]
#                             [--remote <name>] [--base <branch>] [--upstream-url <url>]
#
# Flow:
#   1. Resolve the upstream tag (latest release unless --tag) and stop if its commit
#      is already in <remote>/<base>.
#   2. Merge it with --no-ff into sync/upstream-<tag> as Lucian.
#   3. Build and test with scripts/fork/build.sh (the commands fork-ci runs).
#   4. Push, open the PR, and enable auto-merge with a merge commit. Squash or rebase
#      would drop upstream history, so the next upstream merge would conflict.
#   On a conflict or failed check nothing is pushed: an issue is opened or updated
#   for loadinglucian and the script exits 1 so the workflow run ends red, which is
#   what sends GitHub's failure email. It also exits 1 when any open sync PR has
#   failing checks.
#
# --dry-run performs the merge and checks locally but makes no remote writes; it
# prints the issue it would file. Upstream tags are fetched into refs/fork-sync/*,
# never refs/tags/*, so a later `git push --tags` cannot copy them to this repo.
#
# Environment:
#   GH_TOKEN          Token for remote writes (SYNC_TOKEN in CI). Locally gh's own
#                     login is used when unset.
#   SYNC_READ_TOKEN   Optional token for read-only API calls (CI passes GITHUB_TOKEN,
#                     which can read check results).
#   FORK_SYNC_CHECK   Optional shell command replacing the build/test steps (tests).
#
# Run Go through mise: `mise exec -- scripts/fork/sync.sh --dry-run`.
set -euo pipefail

readonly upstream_repo="router-for-me/CLIProxyAPI"
readonly author_name="Lucian Behind The Scenes"
readonly author_email="loadinglucian@pm.me"
readonly assignee="loadinglucian"

tag=""
dry_run=0
repo="${GITHUB_REPOSITORY:-}"
remote="origin"
base="main"
upstream_url="https://github.com/${upstream_repo}.git"

usage() { sed -n '2,31p' "$0" >&2; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--tag) tag="${2:?--tag needs a value}"; shift 2 ;;
	--dry-run) dry_run=1; shift ;;
	--repo) repo="${2:?--repo needs a value}"; shift 2 ;;
	--remote) remote="${2:?--remote needs a value}"; shift 2 ;;
	--base) base="${2:?--base needs a value}"; shift 2 ;;
	--upstream-url) upstream_url="${2:?--upstream-url needs a value}"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) printf 'unknown argument: %s\n' "$1" >&2; usage; exit 2 ;;
	esac
done

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

work_dir="$(mktemp -d)"
orig_ref=""
failing_prs=""

# --- output helpers ---------------------------------------------------------

# summary appends Markdown to the Actions step summary and echoes it to the log.
summary() {
	if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
		printf '%s\n' "$@" >>"$GITHUB_STEP_SUMMARY"
	fi
	printf '%s\n' "$@"
}

error() {
	if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
		printf '::error::%s\n' "$*"
	else
		printf 'error: %s\n' "$*" >&2
	fi
}

die() { error "$*"; exit 1; }

# gh_read runs read-only gh calls with SYNC_READ_TOKEN when provided, because the
# write token (a fine-grained PAT) has no Checks permission.
gh_read() {
	if [[ -n "${SYNC_READ_TOKEN:-}" ]]; then
		GH_TOKEN="$SYNC_READ_TOKEN" gh "$@"
	else
		gh "$@"
	fi
}

cleanup() {
	if git rev-parse -q --verify MERGE_HEAD >/dev/null 2>&1; then
		git merge --abort || true
	fi
	if [[ -n "$orig_ref" ]]; then
		git switch --quiet "$orig_ref" 2>/dev/null || git switch --quiet --detach "$orig_ref" 2>/dev/null || true
	fi
	rm -rf "$work_dir"
}
trap cleanup EXIT

# finish ends the run, failing it when an open sync PR has failing checks so the
# maintainer is notified even when this run had nothing new to merge.
finish() {
	if [[ -n "$failing_prs" ]]; then
		summary "" "### Open sync PRs with failing checks" "" "$failing_prs" \
			"" "Fix or close these PRs; see FORK.md for how to resolve a sync."
		error "open sync PR(s) have failing checks: $(printf '%s' "$failing_prs" | tr '\n' ' ')"
		exit 1
	fi
	exit 0
}

# --- steps ------------------------------------------------------------------

resolve_tag() {
	if [[ -z "$tag" ]]; then
		tag="$(gh_read api "repos/${upstream_repo}/releases/latest" --jq .tag_name)"
	fi
	# The tag is interpolated into branch names, titles, and jq filters, so only
	# accept plain release-style names.
	if [[ ! "$tag" =~ ^v[0-9]+(\.[0-9]+)*([.-][0-9A-Za-z]+)*$ ]]; then
		die "refusing unexpected upstream tag name: '$tag'"
	fi
}

# check_open_sync_prs records open sync PRs with failing checks (reported by finish)
# and sets existing_pr to the open PR for this tag's branch, if any.
check_open_sync_prs() {
	local rows number head failed url
	rows="$(gh_read pr list --repo "$repo" --state open --limit 100 \
		--json number,headRefName,url,statusCheckRollup \
		--jq '.[] | select(.headRefName | startswith("sync/upstream-"))
			| [.number, .headRefName,
				([.statusCheckRollup[]?
					| select(((.conclusion // "") | IN("FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE"))
						or ((.state // "") | IN("FAILURE", "ERROR")))] | length),
				.url] | @tsv')"
	existing_pr=""
	while IFS=$'\t' read -r number head failed url; do
		[[ -n "$number" ]] || continue
		if [[ "$head" == "$branch" ]]; then
			existing_pr="$url"
		fi
		if [[ "$failed" -gt 0 ]]; then
			failing_prs+="- ${url} (${head}, ${failed} failing check(s))"$'\n'
		fi
	done <<<"$rows"
}

# run_checks runs each build step separately so a failure names the step.
# Sets failed_step and leaves the output tail in $work_dir/check.log.
run_checks() {
	local -a names cmds
	if [[ -n "${FORK_SYNC_CHECK:-}" ]]; then
		names=("check")
		cmds=("$FORK_SYNC_CHECK")
	else
		names=("build server" "go test" "build plugins")
		cmds=("scripts/fork/build.sh server" "scripts/fork/build.sh test" "scripts/fork/build.sh plugins")
	fi
	failed_step=""
	local i status
	for i in "${!names[@]}"; do
		printf '==> sync check: %s\n' "${names[$i]}"
		: >"$work_dir/check.log"
		set +e
		bash -c "${cmds[$i]}" 2>&1 | tee "$work_dir/check.log"
		status="${PIPESTATUS[0]}"
		set -e
		if [[ "$status" -ne 0 ]]; then
			failed_step="${names[$i]}"
			return 1
		fi
	done
}

# resolution_steps prints the instructions an agent or human follows to finish the
# merge by hand. Kept in sync with the "Resolving a sync conflict" section of FORK.md.
resolution_steps() {
	cat <<EOF
## How to resolve

1. In a clone of ${repo}:
   \`\`\`sh
   git fetch origin ${base}
   git fetch --no-tags ${upstream_url} refs/tags/${tag}:refs/fork-sync/${tag}
   git switch -c ${branch} origin/${base}
   git merge --no-ff -m "chore: merge upstream ${tag}" refs/fork-sync/${tag}
   \`\`\`
2. Resolve any conflicts. Upstream files should end up matching upstream; this repo's own changes live only in new files (see FORK.md). Then \`git commit --no-edit\`.
3. Verify with \`mise exec -- scripts/fork/build.sh all\` and fix any failure in this repo's own files.
4. Push the branch, open a PR titled \`chore: merge upstream ${tag}\`, and merge it with a merge commit (\`gh pr merge --merge\`). Never squash or rebase a sync PR: that drops upstream history and breaks every later sync.
5. Never push upstream tags (\`refs/fork-sync/*\` or \`refs/tags/*\`) to this repo. Close this issue once the PR merges.
EOF
}

# report_failure files or updates the issue for this tag, writes the step summary,
# and exits 1 so the workflow run fails and GitHub emails the maintainer.
report_failure() {
	local kind="$1" details="$2"
	local title="Upstream ${tag} ${kind}"
	local run_url=""
	if [[ -n "${GITHUB_RUN_ID:-}" ]]; then
		run_url="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
	fi
	local body="$work_dir/issue.md"
	{
		printf 'The upstream sync could not merge [%s](https://github.com/%s/releases/tag/%s) into `%s`: %s.\n\n' \
			"$tag" "$upstream_repo" "$tag" "$base" "$kind"
		[[ -n "$run_url" ]] && printf 'Workflow run: %s\n\n' "$run_url"
		printf '%s\n\n' "$details"
		resolution_steps
	} >"$body"

	summary "## ${title}" "" "$details" ""
	if [[ "$dry_run" -eq 1 ]]; then
		summary "Dry run: would open or update issue \"${title}\" assigned to ${assignee} with this body:" ""
		cat "$body"
	else
		local number
		number="$(gh_read issue list --repo "$repo" --state open --search "\"${title}\" in:title" \
			--json number,title --jq ".[] | select(.title == \"${title}\") | .number" | head -n 1)"
		if [[ -n "$number" ]]; then
			gh issue edit "$number" --repo "$repo" --body-file "$body" --add-assignee "$assignee" >/dev/null
			summary "Updated issue #${number}."
		else
			local url
			url="$(gh issue create --repo "$repo" --title "$title" --body-file "$body" --assignee "$assignee")"
			summary "Opened ${url}."
		fi
	fi
	if [[ -n "$failing_prs" ]]; then
		summary "" "### Open sync PRs with failing checks" "" "$failing_prs"
	fi
	die "${title}"
}

# --- main -------------------------------------------------------------------

if [[ -n "${GITHUB_ACTIONS:-}" && "$dry_run" -eq 0 && -z "${GH_TOKEN:-}" ]]; then
	die "GH_TOKEN is empty: add the SYNC_TOKEN repository secret (see FORK.md)"
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
	die "working tree has uncommitted changes; commit or set them aside first"
fi
if [[ -z "$repo" ]]; then
	repo="$(gh_read repo view --json nameWithOwner --jq .nameWithOwner)"
fi

orig_ref="$(git symbolic-ref --quiet --short HEAD || git rev-parse HEAD)"

resolve_tag
branch="sync/upstream-${tag}"
check_open_sync_prs

git fetch --quiet --no-tags "$remote" "$base"
git fetch --quiet --no-tags "$upstream_url" "+refs/tags/${tag}:refs/fork-sync/${tag}"
tag_commit="$(git rev-parse "refs/fork-sync/${tag}^{commit}")"

if git merge-base --is-ancestor "$tag_commit" "${remote}/${base}"; then
	summary "Upstream ${tag} (${tag_commit:0:12}) is already in ${base}; nothing to do."
	finish
fi
if [[ -n "$existing_pr" ]]; then
	summary "Sync PR for ${tag} is already open: ${existing_pr}"
	finish
fi

reuse_remote_branch=0
if git ls-remote --exit-code --heads "$remote" "$branch" >/dev/null 2>&1; then
	# An earlier run pushed the branch but did not get to open the PR.
	reuse_remote_branch=1
	git fetch --quiet --no-tags "$remote" "+refs/heads/${branch}:refs/remotes/${remote}/${branch}"
	git switch --quiet -C "$branch" "${remote}/${branch}"
	summary "Reusing existing branch ${branch} from ${remote}."
else
	git switch --quiet --no-track -C "$branch" "${remote}/${base}"
	if ! git -c user.name="$author_name" -c user.email="$author_email" \
		merge --no-ff --no-edit -m "chore: merge upstream ${tag}" "$tag_commit"; then
		conflicts="$(git diff --name-only --diff-filter=U)"
		git merge --abort
		if [[ -z "$conflicts" ]]; then
			report_failure "merge conflict" "\`git merge\` failed without reporting conflicting paths; check the workflow log."
		fi
		report_failure "merge conflict" "Conflicting files:

$(printf '%s\n' "$conflicts" | sed 's/^/- `/; s/$/`/')"
	fi
fi

if ! run_checks; then
	report_failure "build failure" "Failing step: \`${failed_step}\` (\`scripts/fork/build.sh\`). Last lines of its output:

\`\`\`
$(tail -n 60 "$work_dir/check.log")
\`\`\`"
fi

if [[ "$dry_run" -eq 1 ]]; then
	summary "Dry run: ${tag} merges cleanly into ${base} and passes checks. Would push ${branch}, open \"chore: merge upstream ${tag}\", and enable auto-merge (merge commit)."
	finish
fi

if [[ "$reuse_remote_branch" -eq 0 ]]; then
	git push --quiet "$remote" "HEAD:refs/heads/${branch}"
fi
pr_url="$(gh pr create --repo "$repo" --base "$base" --head "$branch" \
	--title "chore: merge upstream ${tag}" \
	--body "Merges upstream release [${tag}](https://github.com/${upstream_repo}/releases/tag/${tag}) into ${base}." \
	--assignee "$assignee")"
pr_number="${pr_url##*/}"
gh pr merge "$pr_number" --repo "$repo" --auto --merge \
	--subject "chore: merge upstream ${tag} (#${pr_number})" --body ""
summary "Opened ${pr_url} with auto-merge (merge commit) enabled."
finish

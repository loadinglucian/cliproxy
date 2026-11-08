#!/usr/bin/env bash
# Merges the latest upstream CLIProxyAPI release into this repo's main branch through
# a pull request. fork-sync.yml runs it every 6 hours; it also runs locally.
#
# Usage: scripts/fork/sync.sh [--tag <tag>] [--dry-run] [--repo <owner/name>]
#                             [--remote <name>] [--base <branch>] [--upstream-url <url>]
#                             [--phase all|check|publish] [--result-dir <dir>]
#
# Flow:
#   1. Resolve the upstream tag (latest release unless --tag) and stop if its commit
#      is already in <remote>/<base>.
#   2. Merge it with --no-ff into sync/upstream-<tag> as Lucian.
#   3. Build and test with scripts/fork/build.sh (the commands fork-ci runs).
#   4. Push, open the PR, and enable auto-merge with a merge commit. Squash or rebase
#      would drop upstream history, so the next upstream merge would conflict.
#      When the PR is already open, only enable auto-merge if it is still off.
#   On a conflict or failed check nothing is pushed: an issue is opened or updated
#   for loadinglucian and the script exits 1 so the workflow run ends red, which is
#   what sends GitHub's failure email. It also exits 1 when any open sync PR has
#   failing checks.
#
# Phases keep the write token away from upstream code. Step 3 runs code no human
# has reviewed, so fork-sync.yml runs it in a job without write credentials:
#   check    Steps 1-3 only, never writes remotely. Records the outcome and the
#            tested commits in <result-dir>/result.env (plus details.md) and exits 0.
#   publish  Redoes steps 1-2 (git only, no repository code runs), requires
#            <result-dir> to describe exactly that merge, then does step 4 or files
#            the issue. Never runs the build.
#   all      Everything in one process (default, for local runs).
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
phase="all"
result_dir=""

usage() { sed -n '2,42p' "$0" >&2; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--tag) tag="${2:?--tag needs a value}"; shift 2 ;;
	--dry-run) dry_run=1; shift ;;
	--repo) repo="${2:?--repo needs a value}"; shift 2 ;;
	--remote) remote="${2:?--remote needs a value}"; shift 2 ;;
	--base) base="${2:?--base needs a value}"; shift 2 ;;
	--upstream-url) upstream_url="${2:?--upstream-url needs a value}"; shift 2 ;;
	--phase) phase="${2:?--phase needs a value}"; shift 2 ;;
	--result-dir) result_dir="${2:?--result-dir needs a value}"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) printf 'unknown argument: %s\n' "$1" >&2; usage; exit 2 ;;
	esac
done

case "$phase" in
all) ;;
check | publish)
	[[ -n "$result_dir" ]] || { printf -- '--phase %s needs --result-dir\n' "$phase" >&2; exit 2; }
	result_dir="$(mkdir -p "$result_dir" && cd "$result_dir" && pwd)"
	;;
*) printf 'unknown phase: %s\n' "$phase" >&2; exit 2 ;;
esac

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

work_dir="$(mktemp -d)"
orig_ref=""
failing_prs=""
merge_started=0
branch_created=0
check_outcome=""

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

# cleanup aborts only a merge this run started, never one the caller left in progress,
# and deletes the local sync branch only when this run created it.
cleanup() {
	if [[ "$merge_started" -eq 1 ]] && git rev-parse -q --verify MERGE_HEAD >/dev/null 2>&1; then
		git merge --abort || true
	fi
	if [[ -n "$orig_ref" ]]; then
		git switch --quiet "$orig_ref" 2>/dev/null || git switch --quiet --detach "$orig_ref" 2>/dev/null || true
	fi
	if [[ "$branch_created" -eq 1 ]]; then
		git branch --quiet -D "$branch" 2>/dev/null || true
	fi
	rm -rf "$work_dir"
}
trap cleanup EXIT

# write_result records the check phase's outcome for the publish phase: $1 is ok,
# skip, merge-conflict, or build-failure; $2 is optional Markdown details.
write_result() {
	local tree=""
	if [[ "$1" == "ok" || "$1" == "build-failure" ]]; then
		tree="$(git rev-parse "HEAD^{tree}")"
	fi
	{
		printf 'outcome=%s\n' "$1"
		printf 'tag=%s\n' "$tag"
		printf 'base_sha=%s\n' "${base_sha:-}"
		printf 'tag_sha=%s\n' "${tag_commit:-}"
		printf 'tree=%s\n' "$tree"
	} >"$result_dir/result.env"
	printf '%s\n' "${2:-}" >"$result_dir/details.md"
	summary "Check phase outcome: $1"
}

# read_result_field prints field $1 from the check result. The file comes from a job
# that ran upstream code, so it is only ever compared against values computed here.
read_result_field() {
	sed -n "s/^${1}=//p" "$result_dir/result.env" | head -n 1
}

# require_check_result makes the publish phase act only on the exact merge the check
# phase built and tested: same upstream tag and commit, same base commit, same tree.
# Sets check_outcome to the recorded outcome (ok or build-failure). Must run in the
# main shell, not a command substitution, so its die ends the script.
require_check_result() {
	[[ -f "$result_dir/result.env" ]] || die "no check result in ${result_dir}"
	local outcome
	outcome="$(read_result_field outcome)"
	if [[ "$(read_result_field tag)" != "$tag" ||
		"$(read_result_field base_sha)" != "$base_sha" ||
		"$(read_result_field tag_sha)" != "$tag_commit" ||
		"$(read_result_field tree)" != "$(git rev-parse "HEAD^{tree}")" ]]; then
		die "check result (${outcome:-none}) does not describe this merge of ${tag} onto ${base_sha:0:12}; rerun fork-sync"
	fi
	case "$outcome" in
	ok | build-failure) check_outcome="$outcome" ;;
	*) die "check result has unexpected outcome '${outcome}' for a clean merge of ${tag}" ;;
	esac
}

# finish ends the run, failing it when an open sync PR has failing checks so the
# maintainer is notified even when this run had nothing new to merge. The check
# phase only records that it stopped; the publish phase reports.
finish() {
	if [[ "$phase" == "check" ]]; then
		write_result skip
		exit 0
	fi
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
# and sets existing_pr to the open PR for this tag's branch, if any, and
# existing_pr_auto to whether auto-merge is enabled on it. Only PRs from this
# repository's own branches count: anyone can open a PR from a fork whose branch is
# named sync/upstream-<tag>, and this script must never enable auto-merge on it.
check_open_sync_prs() {
	local rows number head failed auto url
	rows="$(gh_read pr list --repo "$repo" --state open --base "$base" --limit 100 \
		--json number,headRefName,url,statusCheckRollup,autoMergeRequest,isCrossRepository \
		--jq '.[] | select((.isCrossRepository | not) and (.headRefName | startswith("sync/upstream-")))
			| [.number, .headRefName,
				([.statusCheckRollup[]?
					| select(((.conclusion // "") | IN("FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE"))
						or ((.state // "") | IN("FAILURE", "ERROR")))] | length),
				(.autoMergeRequest != null), .url] | @tsv')"
	existing_pr=""
	existing_pr_auto=""
	while IFS=$'\t' read -r number head failed auto url; do
		[[ -n "$number" ]] || continue
		if [[ "$head" == "$branch" ]]; then
			existing_pr="$url"
			existing_pr_auto="$auto"
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

# enable_auto_merge turns on auto-merge with a merge commit for the sync PR at URL $1.
# Squash or rebase would drop the upstream parent and break every later sync.
enable_auto_merge() {
	local number="${1##*/}"
	gh pr merge "$number" --repo "$repo" --auto --merge \
		--subject "chore: merge upstream ${tag} (#${number})" --body ""
}

# report_failure files or updates the issue for this tag, writes the step summary,
# and exits 1 so the workflow run fails and GitHub emails the maintainer. The check
# phase only records the failure and exits 0 so the publish phase can report it.
report_failure() {
	local kind="$1" details="$2"
	if [[ "$phase" == "check" ]]; then
		write_result "${kind// /-}" "$details"
		exit 0
	fi
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

if [[ -n "${GITHUB_ACTIONS:-}" && "$phase" != "check" && "$dry_run" -eq 0 && -z "${GH_TOKEN:-}" ]]; then
	die "GH_TOKEN is empty: add the SYNC_TOKEN repository secret (see FORK.md)"
fi
if git rev-parse -q --verify MERGE_HEAD >/dev/null 2>&1; then
	die "a merge is already in progress; finish or abort it first"
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
	die "working tree has uncommitted changes; commit or set them aside first"
fi
if [[ -z "$repo" ]]; then
	repo="$(gh_read repo view --json nameWithOwner --jq .nameWithOwner)"
fi

orig_ref="$(git symbolic-ref --quiet --short HEAD || git rev-parse HEAD)"

# publish merges the tag the check phase tested, even if upstream released again since.
if [[ "$phase" == "publish" && -z "$tag" && -f "$result_dir/result.env" ]]; then
	tag="$(read_result_field tag)"
fi
resolve_tag
branch="sync/upstream-${tag}"
check_open_sync_prs

git fetch --quiet --no-tags "$remote" "$base"
git fetch --quiet --no-tags "$upstream_url" "+refs/tags/${tag}:refs/fork-sync/${tag}"
tag_commit="$(git rev-parse "refs/fork-sync/${tag}^{commit}")"
base_sha="$(git rev-parse "${remote}/${base}^{commit}")"

if git merge-base --is-ancestor "$tag_commit" "${remote}/${base}"; then
	summary "Upstream ${tag} (${tag_commit:0:12}) is already in ${base}; nothing to do."
	finish
fi
if [[ -n "$existing_pr" ]]; then
	summary "Sync PR for ${tag} is already open: ${existing_pr}"
	# An earlier run may have opened the PR but failed before enabling auto-merge.
	if [[ "$existing_pr_auto" != "true" && "$phase" != "check" ]]; then
		if [[ "$dry_run" -eq 1 ]]; then
			summary "Dry run: would enable auto-merge (merge commit) on ${existing_pr}."
		else
			enable_auto_merge "$existing_pr"
			summary "Enabled auto-merge (merge commit) on ${existing_pr}."
		fi
	fi
	finish
fi

# A local branch of this name may hold unpushed conflict-resolution commits.
if git show-ref --verify --quiet "refs/heads/${branch}"; then
	die "local branch ${branch} already exists; push, rename, or delete it first"
fi

reuse_remote_branch=0
if git ls-remote --exit-code --heads "$remote" "$branch" >/dev/null 2>&1; then
	# An earlier run pushed the branch but did not get to open the PR.
	reuse_remote_branch=1
	git fetch --quiet --no-tags "$remote" "+refs/heads/${branch}:refs/remotes/${remote}/${branch}"
	git switch --quiet --no-track -c "$branch" "${remote}/${branch}"
	branch_created=1
	if ! git merge-base --is-ancestor "$tag_commit" HEAD; then
		die "existing branch ${branch} on ${remote} does not contain upstream ${tag}; delete it and rerun"
	fi
	summary "Reusing existing branch ${branch} from ${remote}."
else
	git switch --quiet --no-track -c "$branch" "${remote}/${base}"
	branch_created=1
	merge_started=1
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

if [[ "$phase" == "publish" ]]; then
	require_check_result
	if [[ "$check_outcome" == "build-failure" ]]; then
		# details.md is check-job output; cap it below GitHub's issue body limit.
		report_failure "build failure" "$(head -c 50000 "$result_dir/details.md")"
	fi
	summary "Check phase built and tested this merge (tree $(git rev-parse --short "HEAD^{tree}"))."
elif ! run_checks; then
	report_failure "build failure" "Failing step: \`${failed_step}\` (\`scripts/fork/build.sh\`). Last lines of its output:

\`\`\`
$(tail -n 60 "$work_dir/check.log")
\`\`\`"
fi

if [[ "$phase" == "check" ]]; then
	write_result ok
	exit 0
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
enable_auto_merge "$pr_url"
summary "Opened ${pr_url} with auto-merge (merge commit) enabled."
finish

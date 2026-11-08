#!/usr/bin/env bash
# Disables every active workflow in .github/workflows/ whose file name does not start
# with "fork-". Upstream workflow files arrive with each sync merge and stay
# byte-identical to upstream, so they are switched off through the API instead of
# being edited. A new upstream workflow can still run on the sync PR and on the
# merge push before the next call; fork-ci and fork-sync both call this to keep
# that window short.
#
# Usage: scripts/fork/disable-foreign-workflows.sh [--dry-run] [--repo <owner/name>]
# Needs a token with actions: write (GH_TOKEN, or gh's own login locally).
set -euo pipefail

dry_run=0
repo="${GITHUB_REPOSITORY:-}"
while [[ $# -gt 0 ]]; do
	case "$1" in
	--dry-run) dry_run=1; shift ;;
	--repo) repo="${2:?--repo needs a value}"; shift 2 ;;
	*) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
	esac
done
if [[ -z "$repo" ]]; then
	repo="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
fi

# Only file-backed workflows are considered; GitHub's dynamic ones (Dependabot,
# code scanning) have paths outside .github/workflows/.
rows="$(gh api --paginate "repos/${repo}/actions/workflows" \
	--jq '.workflows[] | select(.state == "active")
		| select(.path | startswith(".github/workflows/"))
		| select(.path | startswith(".github/workflows/fork-") | not)
		| [.id, .path] | @tsv')"

if [[ -z "$rows" ]]; then
	echo "No active non-fork workflows."
	exit 0
fi

while IFS=$'\t' read -r id path; do
	if [[ "$dry_run" -eq 1 ]]; then
		echo "Dry run: would disable ${path} (${id})"
	else
		gh api --method PUT "repos/${repo}/actions/workflows/${id}/disable" >/dev/null
		echo "Disabled ${path} (${id})"
	fi
done <<<"$rows"

#!/usr/bin/env bash
# Build and test commands shared by fork-ci, fork-release, fork-sync, and local use.
# CI, release, and sync all call this script so a sync merge is checked with exactly
# the commands CI will run on the resulting PR.
#
# Usage: scripts/fork/build.sh <command> [args]
#   server [out]      Build ./cmd/server with CGO (default out: bin/cli-proxy-api).
#                     Reads VERSION, COMMIT, BUILD_DATE for the upstream ldflags.
#   test              Run go test ./... for the root module, retrying only the
#                     quarantined tests in scripts/fork/flaky-tests.txt and
#                     running scripts/fork/broken-tests.txt once, never failing.
#   plugins [outdir]  Test and build every plugins/*/ directory as a c-shared
#                     library (default outdir: bin/plugins). No-op without plugins/.
#                     Sets main.proxyVersion from VERSION (no-op if undefined).
#                     Plugins inside the root module are covered by `test`.
#   all               server, test, then plugins, with default outputs.
#
# Run Go through mise (an activated shell or `mise exec -- scripts/fork/build.sh all`)
# so the toolchain matches mise.toml.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

log() { printf '==> %s\n' "$*" >&2; }

build_server() {
	local out="${1:-bin/cli-proxy-api}"
	local version="${VERSION:-dev}" commit="${COMMIT:-none}" build_date="${BUILD_DATE:-unknown}"
	mkdir -p "$(dirname "$out")"
	log "building ./cmd/server -> $out (version $version)"
	# Same flags as upstream's release.yaml and Dockerfile. CGO stays on because the
	# dynamic-library plugin loader needs it.
	CGO_ENABLED=1 go build -buildvcs=false \
		-ldflags="-s -w -X main.Version=${version} -X main.Commit=${commit} -X main.BuildDate=${build_date}" \
		-o "$out" ./cmd/server
}

# Upstream tests known to flake, one "./<package-dir> <TestName>" per line. The
# "Flaky test quarantine" section of FORK.md says when to add or remove an entry.
flaky_list="scripts/fork/flaky-tests.txt"
quarantine_attempts=3

# Upstream tests that fail deterministically on upstream's own tree, same format.
# The "Known-broken upstream tests" section of FORK.md says when to add or remove one.
broken_list="scripts/fork/broken-tests.txt"

# warn prints a warning, as an annotation and a step summary line in GitHub Actions.
warn() {
	if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
		printf '::warning::%s\n' "$*"
	else
		printf 'warning: %s\n' "$*" >&2
	fi
	if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
		printf -- '- Warning: %s\n' "$*" >>"$GITHUB_STEP_SUMMARY"
	fi
}

# load_list <file> <kind> fills the parallel arrays l_pkgs and l_names from a test
# list. An entry whose test no longer exists upstream is dropped with a warning, so
# an upstream rename or deletion never fails CI. A malformed line fails.
load_list() {
	local list="$1" kind="$2" line pkg name extra n=0
	l_pkgs=()
	l_names=()
	[[ -f "$list" ]] || return 0
	while IFS= read -r line || [[ -n "$line" ]]; do
		n=$((n + 1))
		pkg="" name="" extra=""
		read -r pkg name extra <<<"${line%%#*}" || true
		[[ -n "$pkg" ]] || continue
		pkg="${pkg%/}"
		if [[ -z "$name" || -n "$extra" || "$pkg" != ./* || ! "$name" =~ ^Test[A-Za-z0-9_]*$ ]]; then
			printf '%s:%d: want "./<package-dir> <TestName>", got: %s\n' "$list" "$n" "$line" >&2
			return 1
		fi
		# Go only runs top-level "func TestName(" declarations in _test.go files.
		if ! grep -qs "^func ${name}(" "$pkg"/*_test.go; then
			warn "$kind test $name no longer exists in $pkg; remove it from $list"
			continue
		fi
		l_pkgs+=("$pkg")
		l_names+=("$name")
	done <"$list"
}

# load_lists fills q_pkgs and q_names from flaky_list, and b_pkgs and b_names from
# broken_list.
load_lists() {
	local -a l_pkgs l_names
	load_list "$flaky_list" quarantined
	q_pkgs=(${l_pkgs[@]+"${l_pkgs[@]}"})
	q_names=(${l_names[@]+"${l_names[@]}"})
	load_list "$broken_list" known-broken
	b_pkgs=(${l_pkgs[@]+"${l_pkgs[@]}"})
	b_names=(${l_names[@]+"${l_names[@]}"})
}

# is_listed succeeds when <package-dir> <TestName> is a loaded entry of either list.
is_listed() {
	local i
	for i in "${!q_names[@]}"; do
		[[ "${q_pkgs[$i]}" == "$1" && "${q_names[$i]}" == "$2" ]] && return 0
	done
	for i in "${!b_names[@]}"; do
		[[ "${b_pkgs[$i]}" == "$1" && "${b_names[$i]}" == "$2" ]] && return 0
	done
	return 1
}

# listed_names prints every loaded test name from both lists, once each.
listed_names() {
	printf '%s\n' ${q_names[@]+"${q_names[@]}"} ${b_names[@]+"${b_names[@]}"} | sort -u
}

# run_name_collisions runs, once and without retry, every test that shares a listed
# name but is not listed itself: -skip matches test names in all packages, so the
# main run skipped it too.
run_name_collisions() {
	local name file dir pkg pkgs="" status=0
	while IFS= read -r name; do
		while IFS= read -r file; do
			dir="$(dirname "$file")"
			is_listed "$dir" "$name" && continue
			# Only packages go test ./... covers (no nested modules or testdata).
			[[ -n "$pkgs" ]] || pkgs="$(go list ./...)"
			pkg="$(go list "$dir" 2>/dev/null)" || continue
			grep -qxF "$pkg" <<<"$pkgs" || continue
			log "running $dir $name once: it shares a listed test's name"
			go test -count=1 -run "^${name}\$" "$dir" || status=1
		done < <(grep -rl --include='*_test.go' "^func ${name}(" . | sort -u || true)
	done < <(listed_names)
	return "$status"
}

# run_quarantined runs each quarantined test alone, up to quarantine_attempts
# times, and fails only when a test fails every attempt. Failed attempts print
# their output; the step summary lists which tests needed a retry. Attempts use
# GOMAXPROCS=1 (-cpu 1) because most of these tests race their own goroutines;
# one failed 3 of 3 default attempts on a GitHub runner and 0 of 400 at -cpu 1.
run_quarantined() {
	local i pkg name attempt out result rows="" retried="" failed=0
	out="$(mktemp)"
	for i in "${!q_names[@]}"; do
		pkg="${q_pkgs[$i]}"
		name="${q_names[$i]}"
		result=""
		for ((attempt = 1; attempt <= quarantine_attempts; attempt++)); do
			if go test -count=1 -cpu 1 -run "^${name}\$" "$pkg" >"$out" 2>&1; then
				result="passed"
				break
			fi
			log "quarantined $pkg $name failed attempt $attempt/$quarantine_attempts:"
			cat "$out"
		done
		if [[ -z "$result" ]]; then
			result="failed all $quarantine_attempts attempts"
			failed=$((failed + 1))
			retried="${retried:+$retried, }\`$name\` (failed)"
		elif [[ "$attempt" -gt 1 ]]; then
			result="passed on attempt $attempt/$quarantine_attempts"
			retried="${retried:+$retried, }\`$name\`"
		fi
		log "quarantined $pkg $name: $result"
		rows="$rows| \`$name\` | \`$pkg\` | $result |"$'\n'
	done
	rm -f "$out"
	if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
		{
			printf '### Quarantined tests (%s)\n\n' "$flaky_list"
			printf '| Test | Package | Result |\n| --- | --- | --- |\n%s\n' "$rows"
			printf 'Needed a retry: %s\n' "${retried:-none}"
		} >>"$GITHUB_STEP_SUMMARY"
	fi
	if [[ "$failed" -gt 0 ]]; then
		log "$failed quarantined test(s) failed every attempt"
		return 1
	fi
}

# run_broken runs each known-broken test once. It never fails: a pass means upstream
# fixed the test, so it warns that the entry should go; a failure is expected.
run_broken() {
	local i pkg name out
	out="$(mktemp)"
	for i in "${!b_names[@]}"; do
		pkg="${b_pkgs[$i]}"
		name="${b_names[$i]}"
		# A short timeout, so a hanging entry is logged instead of eating the job's time limit.
		if go test -count=1 -timeout 2m -run "^${name}\$" "$pkg" >"$out" 2>&1; then
			warn "known-broken test $name in $pkg passed, so upstream fixed it; remove it from $broken_list"
		else
			log "known-broken $pkg $name failed as expected:"
			cat "$out"
		fi
	done
	rm -f "$out"
}

# run_tests runs the root module's suite with every listed test skipped, then each
# quarantined test on its own with retries, then each known-broken test once without
# failing. Every other test gets one run, so a new flake or a real regression fails
# the job at once.
run_tests() {
	local -a q_pkgs q_names b_pkgs b_names args
	load_lists
	args=()
	if [[ $((${#q_names[@]} + ${#b_names[@]})) -gt 0 ]]; then
		args=(-skip "^($(listed_names | paste -sd '|' -))\$")
		log "skipping ${#q_names[@]} quarantined test(s) listed in $flaky_list"
		log "skipping ${#b_names[@]} known-broken test(s) listed in $broken_list"
	fi
	log "running go test ./..."
	go test ${args[@]+"${args[@]}"} ./...
	[[ ${#args[@]} -gt 0 ]] || return 0
	run_name_collisions
	if [[ ${#q_names[@]} -gt 0 ]]; then
		log "running quarantined tests, up to $quarantine_attempts attempts each"
		run_quarantined
	fi
	if [[ ${#b_names[@]} -gt 0 ]]; then
		log "running known-broken tests once each; failures are expected"
		run_broken
	fi
}

plugin_ext() {
	case "$(go env GOOS)" in
	darwin) echo dylib ;;
	windows) echo dll ;;
	*) echo so ;;
	esac
}

build_plugins() {
	local outdir="${1:-bin/plugins}"
	if [[ ! -d plugins ]]; then
		log "no plugins/ directory, skipping plugin builds"
		return 0
	fi
	mkdir -p "$outdir"
	outdir="$(cd "$outdir" && pwd)"

	local ext goarch cflags=""
	ext="$(plugin_ext)"
	goarch="$(go env GOARCH)"
	# Baseline CPU flags from upstream's examples/plugin/Makefile so a plugin runs on
	# any machine the server binary runs on.
	case "$goarch" in
	amd64) cflags="-march=x86-64" ;;
	arm64) cflags="-march=armv8-a" ;;
	esac

	local dir name found=0
	for dir in plugins/*/; do
		[[ -d "$dir" ]] || continue
		name="$(basename "$dir")"
		if ! compgen -G "${dir}*.go" >/dev/null; then
			# Fail instead of skipping so a plugin is never silently left out of CI
			# or a release.
			printf 'plugin %s has no Go files at its root; build.sh only knows Go c-shared plugins\n' "$name" >&2
			return 1
		fi
		found=1
		(
			cd "$dir"
			# A plugin with its own go.mod is outside the root module, so the root
			# go test ./... does not cover it.
			if [[ -f go.mod ]]; then
				log "testing plugin $name"
				go test ./...
			fi
			log "building plugin $name -> $outdir/$name.$ext"
			CGO_ENABLED=1 GOAMD64="${GOAMD64:-v1}" GOARM64="${GOARM64:-v8.0}" \
				CGO_CFLAGS="${cflags}${CGO_CFLAGS:+ $CGO_CFLAGS}" \
				go build -buildvcs=false -buildmode=c-shared \
				-ldflags="-s -w -X main.proxyVersion=${VERSION:-dev}" \
				-o "$outdir/$name.$ext" .
			rm -f "$outdir/$name.h"
		)
	done
	if [[ "$found" -eq 0 ]]; then
		log "plugins/ has no plugin directories, nothing to build"
	fi
}

cmd="${1:-}"
[[ $# -gt 0 ]] && shift
case "$cmd" in
server) build_server "$@" ;;
test) run_tests ;;
plugins) build_plugins "$@" ;;
all)
	build_server
	run_tests
	build_plugins
	;;
*)
	sed -n '2,19p' "$0" >&2
	exit 2
	;;
esac

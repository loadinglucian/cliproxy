#!/usr/bin/env bash
# Build and test commands shared by fork-ci, fork-release, fork-sync, and local use.
# CI, release, and sync all call this script so a sync merge is checked with exactly
# the commands CI will run on the resulting PR.
#
# Usage: scripts/fork/build.sh <command> [args]
#   server [out]      Build ./cmd/server with CGO (default out: bin/cli-proxy-api).
#                     Reads VERSION, COMMIT, BUILD_DATE for the upstream ldflags.
#   test              Run go test ./... for the root module.
#   plugins [outdir]  Test and build every plugins/*/ directory as a c-shared
#                     library (default outdir: bin/plugins). No-op without plugins/.
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

run_tests() {
	log "running go test ./..."
	go test ./...
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
				go build -buildvcs=false -buildmode=c-shared -o "$outdir/$name.$ext" .
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
	sed -n '2,15p' "$0" >&2
	exit 2
	;;
esac

#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"
deps="$(mktemp)"
trap 'rm -f "$deps"' EXIT
GOSUMDB=off GOPROXY=off go list -deps ./cmd/... >"$deps"
status=0
for directory in internal/*; do
	[[ -d "$directory" ]] || continue
	package="github.com/envpilot/runner/$directory"
	if ! grep -Fq "$package" "$deps"; then
		echo "production entrypoint does not reach $package" >&2
		status=1
	fi
done
exit "$status"

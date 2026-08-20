#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
check="$repo_root/scripts/check-brand.sh"
fixture_dir="$(mktemp -d)"
trap 'rm -rf "$fixture_dir"' EXIT

printf '%s\n' 'EnvPlane' 'ENVPLANE_API_TOKEN' 'envplane.io/environment-id' > "$fixture_dir/allowed.txt"
"$check" "$fixture_dir/allowed.txt"

printf '%s%s\n' 'Env' 'Pilot' > "$fixture_dir/forbidden.txt"
if "$check" "$fixture_dir/forbidden.txt"; then
  echo "brand check accepted a deprecated user-facing name" >&2
  exit 1
fi

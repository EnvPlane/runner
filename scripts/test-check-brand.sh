#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
check="$repo_root/scripts/check-brand.sh"
fixture_dir="$(mktemp -d)"
trap 'rm -rf "$fixture_dir"' EXIT

git -C "$fixture_dir" init -q
git -C "$fixture_dir" config user.email test@example.invalid
git -C "$fixture_dir" config user.name brand-test

printf '%s\n' 'ENVPLANE_API_TOKEN' 'envplane.io/environment-id' 'github.com/envplane/contracts' > "$fixture_dir/allowed.md"
git -C "$fixture_dir" add allowed.md
git -C "$fixture_dir" commit -qm baseline
base="$(git -C "$fixture_dir" rev-parse HEAD)"

printf '%s\n' 'ENVPLANE_API_TOKEN' 'envplane.io/environment-id' 'github.com/envplane/contracts' >> "$fixture_dir/allowed.md"
git -C "$fixture_dir" add allowed.md
git -C "$fixture_dir" commit -qm allowed
"$check" --diff-base "$base" "$fixture_dir"

printf '%s\n' 'EnvPlane' >> "$fixture_dir/allowed.md"
git -C "$fixture_dir" add allowed.md
git -C "$fixture_dir" commit -qm forbidden
if "$check" --diff-base HEAD^ "$fixture_dir"; then
  echo "brand check accepted new human-readable EnvPlane text" >&2
  exit 1
fi

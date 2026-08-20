#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --diff-base [base-revision] [target-directory]" >&2
}

if [[ "${1:-}" != "--diff-base" ]]; then
  usage
  exit 2
fi

base="${2:-}"
target="${3:-$(git rev-parse --show-toplevel)}"
if [[ ! -d "$target" ]]; then
  echo "brand check target does not exist: $target" >&2
  exit 2
fi

if [[ -z "$base" ]]; then
  if [[ "${GITHUB_EVENT_NAME:-}" == "pull_request" && -n "${GITHUB_BASE_REF:-}" ]]; then
    base="$(git -C "$target" merge-base HEAD "origin/$GITHUB_BASE_REF")"
  elif [[ "${GITHUB_EVENT_BEFORE:-}" =~ ^[0-9a-f]{40}$ && ! "${GITHUB_EVENT_BEFORE}" =~ ^0+$ ]]; then
    base="$GITHUB_EVENT_BEFORE"
  else
    base="$(git -C "$target" rev-parse HEAD^)"
  fi
fi

if ! git -C "$target" rev-parse --verify "$base^{commit}" >/dev/null 2>&1; then
  echo "brand check base revision is unavailable: $base" >&2
  exit 2
fi

added_lines="$(git -C "$target" diff --no-ext-diff --unified=0 --no-color "$base" HEAD -- . \
  ':(exclude)scripts/check-brand.sh' \
  ':(exclude)scripts/test-check-brand.sh' \
  | sed -n '/^+/ { /^+++/!p; }' || true)"

if command -v rg >/dev/null 2>&1; then
  violations="$(printf '%s\n' "$added_lines" | rg -n 'EnvPlane' || true)"
else
  violations="$(printf '%s\n' "$added_lines" | grep -n 'EnvPlane' || true)"
fi
if [[ -n "$violations" ]]; then
  printf '%s\n' "$violations" >&2
  echo "new human-readable EnvPlane text is forbidden; use ENVPLANE_* or lowercase envplane compatibility identifiers" >&2
  exit 1
fi

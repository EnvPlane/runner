#!/usr/bin/env bash
set -euo pipefail

target="${1:-}"
if [[ -z "$target" ]]; then
  target="$(git rev-parse --show-toplevel)"
fi

if [[ ! -e "$target" ]]; then
  echo "brand check target does not exist: $target" >&2
  exit 2
fi

deprecated_brand="$(printf '%s%s' 'Env' 'Pilot')"

search_deprecated_brand() {
  if command -v rg >/dev/null 2>&1; then
    rg -n --hidden \
      -g '!**/.git/**' \
      -g '!node_modules/**' \
      -g '!**/.next/**' \
      -g '!vendor/**' \
      -g '!dist/**' \
      -g '!build/**' \
      -e "$deprecated_brand" \
      "$target"
    return
  fi

  if [[ -d "$target" ]]; then
    grep -RIn \
      --exclude-dir=.git \
      --exclude-dir=node_modules \
      --exclude-dir=.next \
      --exclude-dir=vendor \
      --exclude-dir=dist \
      --exclude-dir=build \
      -- "$deprecated_brand" \
      "$target"
    return
  fi

  grep -n -- "$deprecated_brand" "$target"
}

set +e
search_deprecated_brand
search_status=$?
set -e

if [[ "$search_status" -eq 0 ]]; then
  echo "Use the EnvPlane product name and the ENVPLANE_*/envplane.io/* identifiers consistently." >&2
  exit 1
fi

if [[ "$search_status" -ne 1 ]]; then
  echo "brand check failed while searching $target" >&2
  exit "$search_status"
fi

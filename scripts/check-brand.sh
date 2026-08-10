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

if rg -n --hidden \
  -g '!.git/**' \
  -g '!node_modules/**' \
  -g '!**/.next/**' \
  -g '!vendor/**' \
  -g '!dist/**' \
  -g '!build/**' \
  -e "$deprecated_brand" \
  "$target"; then
  echo "Use the canonical EnvPlane product name in human-readable text. Legacy machine identifiers such as ENVPILOT_* and envpilot.io/* remain supported." >&2
  exit 1
fi

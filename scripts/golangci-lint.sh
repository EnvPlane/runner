#!/usr/bin/env bash
set -euo pipefail

lint_version="${GOLANGCI_LINT_VERSION:-v1.64.8}"
module_file="${GO_VERSION_FILE:-go.mod}"
required_go="${GO_MIN_VERSION:-}"
if [[ -z "${required_go}" ]]; then
  required_go="$(awk '/^go[[:space:]]/ { print $2; exit }' "${module_file}")"
fi
if [[ -z "${required_go}" ]]; then
  required_go="1.25.0"
fi

normalize_version() {
  local version="$1"
  if [[ "$version" =~ ^[0-9]+\.[0-9]+$ ]]; then
    version="${version}.0"
  fi
  printf "%s" "$version"
}

version_ge() {
  local current
  local required
  current="$(normalize_version "$1")"
  required="$(normalize_version "$2")"
  local lowest
  lowest="$(printf '%s\n%s\n' "$required" "$current" | sort -V | head -n 1)"
  [[ "$lowest" == "$required" ]]
}

extract_builder_version() {
  local output="$1"
  if [[ "$output" =~ (go[0-9]+\.[0-9]+(\.[0-9]+)?) ]]; then
    echo "${BASH_REMATCH[1]#go}"
  fi
}

extract_lint_version() {
  local output="$1"
  if [[ "$output" =~ (v[0-9]+\.[0-9]+(\.[0-9]+)?) ]]; then
    echo "${BASH_REMATCH[1]}"
  fi
}

lint_bin="$(go env GOPATH)/bin/golangci-lint"
need_install=0

if [[ ! -x "$lint_bin" ]]; then
  need_install=1
else
  lint_output="$($lint_bin --version)"
  installed_version="$(extract_lint_version "$lint_output")"
  builder_version="$(extract_builder_version "$lint_output")"

  if [[ "$installed_version" != "$lint_version" ]]; then
    need_install=1
  elif [[ -n "$builder_version" ]] && ! version_ge "$builder_version" "$required_go"; then
    need_install=1
  fi
fi

if [[ "$need_install" -eq 1 ]]; then
  go install "github.com/golangci/golangci-lint/cmd/golangci-lint@${lint_version}"
fi

"$lint_bin" run ./...

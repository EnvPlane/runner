#!/usr/bin/env bash
set -euo pipefail

lint_version="${GOLANGCI_LINT_VERSION:-v1.64.8}"
module_file="${GO_VERSION_FILE:-go.mod}"
required_go="${GO_MIN_VERSION:-}"
if [[ -z "${required_go}" ]] && [[ -f "${module_file}" ]]; then
  required_go="$(awk '/^go[[:space:]]/ { print $2; exit }' "${module_file}")"
fi
if [[ -z "${required_go}" ]]; then
  required_go="1.25.0"
fi

if [[ "${GOLANGCI_LINT_FORCE_INSTALL:-1}" != "0" ]]; then
  go install "github.com/golangci/golangci-lint/cmd/golangci-lint@${lint_version}"
else
  # Legacy behavior: keep fast path when preinstalled binary is compatible.
  lint_bin="$(go env GOPATH)/bin/golangci-lint"
  need_install=0

  if [[ ! -x "$lint_bin" ]]; then
    need_install=1
  else
    lint_output="$($lint_bin --version)"
    installed_lint="$(echo "$lint_output" | awk 'match($0, /v[0-9]+\.[0-9]+(\.[0-9]+)?/, m) { print m[0]; exit }')"
    installed_builder="$(echo "$lint_output" | awk 'match($0, /go[0-9]+\.[0-9]+(\.[0-9]+)?/, m) { print m[0]; exit }')"

    if [[ "$installed_lint" != "$lint_version" ]]; then
      need_install=1
    elif [[ -n "$installed_builder" ]]; then
      installed_builder="${installed_builder#go}"
      if ! command -v awk >/dev/null 2>&1; then
        need_install=1
      else
        current="$(printf '%s\n%s\n' "$installed_builder" "$required_go" | sort -V | head -n 1)"
        if [[ "$current" != "$required_go" ]]; then
          need_install=1
        fi
      fi
    fi
  fi

  if [[ "$need_install" -eq 1 ]]; then
    go install "github.com/golangci/golangci-lint/cmd/golangci-lint@${lint_version}"
  fi
fi

"$(go env GOPATH)/bin/golangci-lint" run ./...

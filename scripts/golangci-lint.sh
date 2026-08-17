#!/usr/bin/env bash
set -euo pipefail

lint_version="${GOLANGCI_LINT_VERSION:-v1.64.8}"

installed_version() {
  if command -v golangci-lint >/dev/null 2>&1; then
    golangci-lint --version | awk 'match($0, /v[0-9]+\.[0-9]+\.[0-9]+/, m) { print m[0]; exit }'
  fi
}

if [[ "$(installed_version)" == "${lint_version}" ]]; then
  "$(command -v golangci-lint)" run ./...
  exit 0
fi

go install github.com/golangci/golangci-lint/cmd/golangci-lint@"${lint_version}"

"$(go env GOPATH)/bin/golangci-lint" run ./...

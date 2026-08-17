#!/usr/bin/env bash
set -euo pipefail

lint_version="${GOLANGCI_LINT_VERSION:-v1.59.1}"

if ! command -v golangci-lint >/dev/null 2>&1; then
  go install github.com/golangci/golangci-lint/cmd/golangci-lint@"${lint_version}"
fi

"$(go env GOPATH)/bin/golangci-lint" run ./...

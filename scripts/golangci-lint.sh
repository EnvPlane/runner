#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
lint_version="${GOLANGCI_LINT_VERSION:-v1.64.8}"
module_file="${GO_VERSION_FILE:-go.mod}"
required_go="${GO_MIN_VERSION:-}"
if [[ -z "${required_go}" ]] && [[ -f "${module_file}" ]]; then
  required_go="$(awk '/^go[[:space:]]/ { print $2; exit }' "${module_file}")"
fi
if [[ -z "${required_go}" ]]; then
  required_go="1.25.0"
fi

version_ge() {
  local current="$1"
  local required="$2"
  local lowest
  lowest="$(printf '%s\n%s\n' "${required}" "${current}" | sort -V | head -n 1)"
  [[ "${lowest}" == "${required}" ]]
}

go_version() {
  "$1" version | awk '{print $3}' | sed 's/^go//'
}

resolve_go() {
  local candidate="${GO_BIN:-$(command -v go || true)}"
  if [[ -n "${candidate}" ]]; then
    if [[ ! -x "${candidate}" ]]; then
      echo "--error: GO_BIN is not executable: ${candidate}" >&2
      return 1
    fi
    echo "${candidate}"
    return 0
  fi
  echo "No go binary found in PATH" >&2
  return 1
}

go_bin="$(resolve_go)"
go_ver="$(go_version "${go_bin}")"
if ! version_ge "${go_ver}" "${required_go}"; then
  if [[ -x "${script_dir}/ensure-go.sh" ]]; then
    GO_MIN_VERSION="${required_go}" GO_VERSION_FILE="${module_file}" "${script_dir}/ensure-go.sh"
    go_bin="$(resolve_go)"
    go_ver="$(go_version "${go_bin}")"
  fi
fi

if ! version_ge "${go_ver}" "${required_go}"; then
  echo "::error::Go version is ${go_ver}, but golangci-lint install requires at least ${required_go}." >&2
  echo "::error::Run ensure-go.sh before scripts/golangci-lint.sh or set GO_BIN explicitly." >&2
  exit 1
fi

if [[ "${GOLANGCI_LINT_FORCE_INSTALL:-1}" != "0" ]]; then
  "${go_bin}" install "github.com/golangci/golangci-lint/cmd/golangci-lint@${lint_version}"
else
  # Legacy behavior: keep fast path when preinstalled binary is compatible.
  lint_bin="${GOPATH:-$(go env GOPATH)}/bin/golangci-lint"
  need_install=0

  if [[ ! -x "${lint_bin}" ]]; then
    need_install=1
  else
    lint_output="$(${lint_bin} --version)"
    installed_lint="$(echo "${lint_output}" | awk 'match($0, /v[0-9]+\.[0-9]+(\.[0-9]+)?/, m) { print m[0]; exit }')"
    installed_builder="$(echo "${lint_output}" | awk 'match($0, /go[0-9]+\.[0-9]+(\.[0-9]+)?/, m) { print m[0]; exit }')"

    if [[ "${installed_lint}" != "${lint_version}" ]]; then
      need_install=1
    elif [[ -n "${installed_builder}" ]]; then
      installed_builder="${installed_builder#go}"
      if ! version_ge "${installed_builder}" "${required_go}"; then
        need_install=1
      fi
    else
      need_install=1
    fi
  fi

  if [[ "${need_install}" -eq 1 ]]; then
    "${go_bin}" install "github.com/golangci/golangci-lint/cmd/golangci-lint@${lint_version}"
  fi
fi

"${lint_bin:=${GOPATH:-$(go env GOPATH)}/bin/golangci-lint}" run ./...

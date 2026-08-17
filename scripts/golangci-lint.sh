#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
lint_version="${GOLANGCI_LINT_VERSION:-v2.12.2}"
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

go_version_from_bin() {
  local bin="$1"
  "$bin" version 2>/dev/null | awk '{print $3}' | sed 's/^go//'
}

go_gopath_from_bin() {
  local bin="$1"
  "$bin" env GOPATH
}

check_go_binary() {
  local bin="$1"
  local ver
  if [[ ! -x "${bin}" ]]; then
    return 1
  fi
  ver="$(go_version_from_bin "${bin}")"
  if [[ -z "${ver}" ]]; then
    return 1
  fi
  if version_ge "${ver}" "${required_go}"; then
    GO_BIN_SELECTED="${bin}"
    GO_VERSION_SELECTED="${ver}"
    GO_GOPATH_SELECTED="$(go_gopath_from_bin "${bin}")"
    return 0
  fi
  return 1
}

ensure_and_select_go() {
  local bin

  GO_BIN_SELECTED=""
  GO_VERSION_SELECTED=""
  GO_GOPATH_SELECTED=""

  if [[ -n "${GO_BIN:-}" ]]; then
    check_go_binary "${GO_BIN}" && return 0
  fi

  bin="$(command -v go || true)"
  if [[ -n "${bin}" ]] && check_go_binary "${bin}"; then
    return 0
  fi

  if check_go_binary "/usr/local/go/bin/go"; then
    return 0
  fi

  if [[ -x "${script_dir}/ensure-go.sh" ]]; then
    GO_MIN_VERSION="${required_go}" GO_VERSION_FILE="${module_file}" "${script_dir}/ensure-go.sh" || true
    if check_go_binary "/usr/local/go/bin/go"; then
      return 0
    fi
  fi

  bin="$(command -v go || true)"
  if [[ -n "${bin}" ]] && check_go_binary "${bin}"; then
    return 0
  fi

  return 1
}

if ! ensure_and_select_go; then
  echo "::error::No suitable go toolchain for golangci-lint. required >= ${required_go}" >&2
  echo "::error::Check GO_BIN, or set GO_MIN_VERSION/GO_VERSION_FILE." >&2
  exit 1
fi

lint_gopath="${GO_GOPATH_SELECTED}"
lint_bin="${lint_gopath}/bin/golangci-lint"

if [[ "${GOLANGCI_LINT_FORCE_INSTALL:-1}" != "0" ]]; then
  rm -f "${lint_bin}"
  "${GO_BIN_SELECTED}" install "github.com/golangci/golangci-lint/cmd/golangci-lint@${lint_version}"
else
  # Legacy behavior: keep fast path when preinstalled binary is compatible.
  need_install=0

  if [[ ! -x "${lint_bin}" ]]; then
    need_install=1
  else
    lint_output="$(${lint_bin} --version 2>&1)"
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
    rm -f "${lint_bin}"
    "${GO_BIN_SELECTED}" install "github.com/golangci/golangci-lint/cmd/golangci-lint@${lint_version}"
  fi
fi

"${lint_bin}" run ./...

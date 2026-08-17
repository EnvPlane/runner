#!/usr/bin/env bash
set -euo pipefail

required_version="${GO_MIN_VERSION:-}"
if [[ -z "${required_version}" ]]; then
  required_version="$(awk '/^go[[:space:]]/ { print $2; exit }' "${GO_VERSION_FILE:-go.mod}")"
fi
if [[ -z "${required_version}" ]]; then
  required_version="1.22.0"
fi

version_ge() {
  local current="$1"
  local required="$2"

  local lowest
  lowest="$(printf '%s\n%s\n' "$required" "$current" | sort -V | head -n 1)"
  [[ "$lowest" == "$required" ]]
}

announce() {
  echo "$1"
}

add_to_path() {
  local go_bin="$1"
  local dir
  dir="$(cd "$(dirname "$go_bin")" && pwd)"
  echo "$dir" >> "${GITHUB_PATH:-/tmp/gh-action-path}"
}

candidate_version_ok() {
  local go_bin="$1"
  if [[ ! -x "$go_bin" ]]; then
    return 1
  fi
  local current_version
  current_version="$("$go_bin" version | awk '{print $3}' | sed 's/^go//')"
  if version_ge "$current_version" "$required_version"; then
    announce "Using Go ${current_version} from ${go_bin}"
    add_to_path "$go_bin"
    "$go_bin" version
    return 0
  fi
  announce "Skipping ${go_bin}: ${current_version} < ${required_version}"
  return 1
}

pick_existing() {
  if command -v go >/dev/null 2>&1; then
    if candidate_version_ok "$(command -v go)"; then
      return 0
    fi
  fi

  local toolcache_root="/opt/hostedtoolcache/go"
  if [[ -d "$toolcache_root" ]]; then
    local version dir go_bin
    for dir in "$toolcache_root"/*; do
      [[ -d "$dir" ]] || continue
      for go_bin in "$dir"/x64/bin/go "$dir"/amd64/bin/go; do
        if [[ -x "$go_bin" ]] && candidate_version_ok "$go_bin"; then
          return 0
        fi
      done
    done
  fi

  local preinstalled="/usr/local/go/bin/go"
  if candidate_version_ok "$preinstalled"; then
    return 0
  fi

  return 1
}

install_from_godl() {
  local arch
  case "$(uname -m)" in
    x86_64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) announce "::error::Unsupported architecture: $(uname -m)"; return 1 ;;
  esac

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  trap 'if [[ -n "${tmp_dir-}" ]]; then rm -rf "$tmp_dir"; fi' EXIT

  local archive="go${required_version}.linux-${arch}.tar.gz"
  local url="https://go.dev/dl/${archive}"
  announce "Downloading Go ${required_version} from ${url}"
  curl -fsSL "$url" -o "$tmp_dir/$archive"

  local target="/usr/local/go"
  if [[ -d "$target" ]]; then
    sudo rm -rf "$target"
  fi
  sudo tar -C /usr/local -xzf "$tmp_dir/$archive"
  if candidate_version_ok "/usr/local/go/bin/go"; then
    return 0
  fi

  return 1
}

if pick_existing; then
  exit 0
fi

announce "::warning::No suitable preinstalled Go runtime (${required_version}+) found. Trying Go download."
if install_from_godl; then
  exit 0
fi

announce "::warning::Go tarball download failed or produced unsupported runtime; trying apt fallback."
sudo apt-get update
sudo apt-get install -y golang-go

if candidate_version_ok "$(command -v go)"; then
  exit 0
fi

announce "::error::Unable to provision Go runtime >= ${required_version}"
exit 1

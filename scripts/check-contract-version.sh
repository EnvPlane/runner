#!/usr/bin/env bash
set -euo pipefail

# Keep the consumer on the latest immutable contracts release.
latest="${ENVPLANE_CONTRACTS_LATEST:-}"
if [[ -z "$latest" ]]; then
  remote="${ENVPLANE_CONTRACTS_REPOSITORY:-https://github.com/EnvPlane/contracts.git}"
  latest="$(git ls-remote --tags --refs "$remote" 'refs/tags/v*' | awk -F/ '{print $3}' | sort -V | tail -n1)"
fi
if [[ -z "$latest" ]]; then
  echo "unable to resolve the latest contracts release" >&2
  exit 1
fi

pinned="$(awk '$1 == "github.com/envplane/contracts" && $2 != "=>" {print $2; exit} $1 == "require" && $2 == "github.com/envplane/contracts" {print $3; exit}' go.mod)"
if [[ -z "$pinned" ]]; then
  echo "go.mod does not declare github.com/envplane/contracts" >&2
  exit 1
fi
if [[ "$pinned" != "$latest" ]]; then
  echo "contracts pin $pinned is not the latest release $latest" >&2
  exit 1
fi

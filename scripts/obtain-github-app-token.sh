#!/usr/bin/env bash
set -euo pipefail

: "${ENVPLANE_AUTOMATION_APP_CLIENT_ID:?missing ENVPLANE_AUTOMATION_APP_CLIENT_ID}"
: "${ENVPLANE_AUTOMATION_APP_PRIVATE_KEY:?missing ENVPLANE_AUTOMATION_APP_PRIVATE_KEY}"

GH_APP_OWNER="${GH_APP_OWNER:-EnvPlane}"
GH_APP_REPOSITORY="${GH_APP_REPOSITORY:-deploy}"
if [[ -z "${GH_APP_TOKEN_PERMISSIONS+x}" || -z "${GH_APP_TOKEN_PERMISSIONS}" ]]; then
  GH_APP_TOKEN_PERMISSIONS='{"contents":"read"}'
fi
GH_API_BASE="https://api.github.com"

# GitHub API calls must never hold a workflow indefinitely.  In particular,
# codeload/API throttling and transient network failures are common on hosted
# runners; bound both connection and total request time while allowing a small
# retry window for retryable responses.
github_api() {
  curl -fsS \
    --connect-timeout 10 \
    --max-time 30 \
    --retry 2 \
    --retry-delay 2 \
    --retry-max-time 45 \
    --retry-connrefused \
    "$@"
}

trim() {
  printf '%s' "$1" | sed -E 's/^[[:space:]]+|[[:space:]]+$//g'
}

strip_matching_quotes() {
  local v="$1"
  v="$(trim "$v")"
  if [[ "${v:0:1}" == "\"" && "${v: -1}" == "\"" && ${#v} -ge 2 ]]; then
    printf '%s' "${v:1:${#v}-2}"
  elif [[ "${v:0:1}" == "'" && "${v: -1}" == "'" && ${#v} -ge 2 ]]; then
    printf '%s' "${v:1:${#v}-2}"
  else
    printf '%s' "$v"
  fi
}

normalize_permissions_json() {
  local value="${1:-}"
  local extracted
  local object_candidate
  local key
  local pair_value
  local pair_json
  local pair_count=0
  local output_pairs=""

  value="${value//$'\r'/}"
  value="$(trim "$value")"

  # Peel single-level wrapper quotes repeatedly.
  while :; do
    if [[ -z "$value" ]]; then
      break
    fi
    local first_char="${value:0:1}"
    local last_char="${value: -1}"
    if [[ "$first_char" == '"' && "$last_char" == '"' && ${#value} -ge 2 ]]; then
      value="${value:1:${#value}-2}"
      value="$(trim "$value")"
      continue
    fi
    if [[ "$first_char" == "'" && "$last_char" == "'" && ${#value} -ge 2 ]]; then
      value="${value:1:${#value}-2}"
      value="$(trim "$value")"
      continue
    fi
    break
  done

  if jq -e -c . <<<"$value" >/dev/null 2>&1; then
    printf '%s' "$value"
    return 0
  fi

  extracted="$(printf '%s' "$value" | awk 'match($0, /\{.*\}/) { print substr($0, RSTART, RLENGTH); exit }')"
  if [[ -n "$extracted" ]] && jq -e -c . <<<"$extracted" >/dev/null 2>&1; then
    printf '%s' "$extracted"
    return 0
  fi

  # Fallback: tolerate unquoted YAML-like map fragments like {contents:read,issues:write}
  if [[ "$value" == *"{"* && "$value" == *"}"* ]]; then
    object_candidate="$(printf '%s' "$value" | awk 'match($0, /\{.*\}/) { print substr($0, RSTART+1, RLENGTH-2); exit }')"
    object_candidate="$(trim "$object_candidate")"
    if [[ -n "$object_candidate" ]]; then
      IFS=',' read -r -a raw_pairs <<< "$object_candidate"
      for trimmed_pair in "${raw_pairs[@]}"; do
        trimmed_pair="$(trim "$trimmed_pair")"
        if [[ -z "$trimmed_pair" ]]; then
          continue
        fi

        if [[ "$trimmed_pair" == *":"* ]]; then
          key="${trimmed_pair%%:*}"
          pair_value="${trimmed_pair#*:}"
        else
          return 1
        fi

        key="$(strip_matching_quotes "$key")"
        if [[ -z "$key" ]] || ! [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
          return 1
        fi

        pair_value="$(strip_matching_quotes "$pair_value")"
        if [[ "$pair_value" == "true" || "$pair_value" == "false" || "$pair_value" == "null" || "$pair_value" == "0" || "$pair_value" == "1" ]]; then
          pair_json="\"$key\":$pair_value"
        else
          pair_json="\"$key\":\"$pair_value\""
        fi

        if [[ $pair_count -gt 0 ]]; then
          output_pairs+=","
        fi
        output_pairs+="$pair_json"
        pair_count=$((pair_count + 1))
      done
    fi
  fi

  if [[ $pair_count -gt 0 ]]; then
    extracted="{${output_pairs}}"
    if jq -e -c . <<<"$extracted" >/dev/null 2>&1; then
      printf '%s' "$extracted"
      return 0
    fi
  fi

  return 1
}

jwt_b64_url() {
  openssl base64 -e -A | tr '/+' '_-' | tr -d '='
}

normalized_key=$(printf '%s' "${ENVPLANE_AUTOMATION_APP_PRIVATE_KEY}" | sed 's/\\n/\n/g')
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
key_path="$tmp_dir/private-key.pem"
printf '%s\n' "$normalized_key" > "$key_path"

now=$(date +%s)
iat=$((now - 60))
exp=$((now + 600))

header='{"alg":"RS256","typ":"JWT"}'
payload=$(jq -n --argjson iat "$iat" --argjson exp "$exp" --arg iss "$ENVPLANE_AUTOMATION_APP_CLIENT_ID" '{iat: ($iat|tonumber), exp: ($exp|tonumber), iss: $iss}')

unsigned="$(printf '%s' "$header" | jwt_b64_url).$(printf '%s' "$payload" | jwt_b64_url)"
signature=$(printf '%s' "$unsigned" | openssl dgst -sha256 -sign "$key_path" | openssl base64 -A | tr '/+' '_-' | tr -d '=')
jwt="${unsigned}.${signature}"

installation=$(github_api \
  -H "Authorization: Bearer ${jwt}" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "$GH_API_BASE/repos/$GH_APP_OWNER/$GH_APP_REPOSITORY/installation")

installation_id=$(jq -r '.id // empty' <<<"$installation")
if [[ -z "$installation_id" || "$installation_id" == "null" ]]; then
  echo "Unable to resolve installation for repository owner/repo" >&2
  jq -r .message <<<"$installation" >&2 || true
  exit 1
fi

if ! GH_APP_TOKEN_PERMISSIONS="$(normalize_permissions_json "$GH_APP_TOKEN_PERMISSIONS")"; then
  echo "Invalid GH_APP_TOKEN_PERMISSIONS JSON value, using default {\\\"contents\\\":\\\"read\\\"}" >&2
  GH_APP_TOKEN_PERMISSIONS='{"contents":"read"}'
fi

permissions_json=$(jq -c -e . <<<"$GH_APP_TOKEN_PERMISSIONS")
if [[ -z "$permissions_json" ]]; then
  echo "Invalid GH_APP_TOKEN_PERMISSIONS JSON value" >&2
  exit 1
fi
if ! jq -e 'type == "object"' <<<"$permissions_json" > /dev/null; then
  echo "GH_APP_TOKEN_PERMISSIONS must be a JSON object" >&2
  exit 1
fi
permissions_payload="{\"permissions\":${permissions_json}}"
access=$(github_api \
  -X POST \
  -H "Authorization: Bearer ${jwt}" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "$GH_API_BASE/app/installations/$installation_id/access_tokens" \
  -d "$permissions_payload")

app_token=$(jq -r '.token // empty' <<<"$access")
if [[ -z "$app_token" || "$app_token" == "null" ]]; then
  echo "Unable to mint GitHub App token" >&2
  jq -r .message <<<"$access" >&2 || true
  exit 1
fi

printf 'token=%s\n' "$app_token" >> "${GITHUB_OUTPUT}"

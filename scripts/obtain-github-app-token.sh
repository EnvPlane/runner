#!/usr/bin/env bash
set -euo pipefail

: "${ENVPILOT_AUTOMATION_APP_CLIENT_ID:?missing ENVPILOT_AUTOMATION_APP_CLIENT_ID}"
: "${ENVPILOT_AUTOMATION_APP_PRIVATE_KEY:?missing ENVPILOT_AUTOMATION_APP_PRIVATE_KEY}"

GH_APP_OWNER="${GH_APP_OWNER:-EnvPlane}"
GH_APP_REPOSITORY="${GH_APP_REPOSITORY:-deploy}"
GH_APP_TOKEN_PERMISSIONS="${GH_APP_TOKEN_PERMISSIONS:-{\"contents\":\"read\"}}"
GH_API_BASE="https://api.github.com"

jwt_b64_url() {
  openssl base64 -e -A | tr '/+' '_-' | tr -d '='
}

normalized_key=$(printf '%s' "${ENVPILOT_AUTOMATION_APP_PRIVATE_KEY}" | sed 's/\\n/\n/g')
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
key_path="$tmp_dir/private-key.pem"
printf '%s\n' "$normalized_key" > "$key_path"

now=$(date +%s)
iat=$((now - 60))
exp=$((now + 600))

header='{"alg":"RS256","typ":"JWT"}'
payload=$(jq -n --argjson iat "$iat" --argjson exp "$exp" --arg iss "$ENVPILOT_AUTOMATION_APP_CLIENT_ID" '{iat: ($iat|tonumber), exp: ($exp|tonumber), iss: $iss}')

unsigned="$(printf '%s' "$header" | jwt_b64_url).$(printf '%s' "$payload" | jwt_b64_url)"
signature=$(printf '%s' "$unsigned" | openssl dgst -sha256 -sign "$key_path" | openssl base64 -A | tr '/+' '_-' | tr -d '=')
jwt="${unsigned}.${signature}"

installation=$(curl -fsS \
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

permissions_payload=$(jq -n --argjson permissions "$GH_APP_TOKEN_PERMISSIONS" '{permissions: $permissions}')
access=$(curl -fsS \
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

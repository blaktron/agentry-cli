#!/usr/bin/env bash
# Exchanges the OIDC token GitHub minted for this job for an Azure access
# token for the Artifact Signing service, and prints THAT token on standard
# output — nothing else goes there, so `token=$(scripts/signing-token.sh)` is
# the whole contract. The caller masks it (`::add-mask::`) and hands it on
# through the environment.
#
# The exchange is the OAuth client-credentials grant with the OIDC token as
# the client assertion, against the Entra app registration whose one
# federated credential names this repository's `release` environment. There
# is no Azure secret: the job's identity is the OIDC token. Both tokens reach
# curl through a config on standard input, never argv, so neither is in a
# process listing on this persistent runner. Diagnostics — the OIDC subject
# Azure will match, and Azure's error when it refuses — go to standard error.
#
#   ACTIONS_ID_TOKEN_REQUEST_URL / _TOKEN   from the job (permissions: id-token: write)
#   AZURE_TENANT_ID, AZURE_CLIENT_ID        repository variables (identifiers)
set -euo pipefail

fail() { echo "signing-token: $*" >&2; exit 1; }
: "${AZURE_TENANT_ID:?the AZURE_TENANT_ID repository variable is not set}"
: "${AZURE_CLIENT_ID:?the AZURE_CLIENT_ID repository variable is not set}"
: "${ACTIONS_ID_TOKEN_REQUEST_URL:?this job has no id-token permission}"
: "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:?this job has no id-token permission}"
command -v jq >/dev/null 2>&1 || fail "jq is needed"

# A body that is not JSON (a proxy's HTML, a 5xx page) must still be readable.
show() { if printf '%s' "$1" | jq -e . >/dev/null 2>&1; then printf '%s' "$1" | jq "$2" >&2; else printf '%s\n' "$1" | head -c 2000 >&2; echo >&2; fi; }

jwt=$(curl -sSf -K - <<CURL
url = "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=api://AzureADTokenExchange"
header = "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}"
CURL
) || fail "GitHub did not issue an OIDC token"
jwt=$(printf '%s' "$jwt" | jq -r '.value // empty')
[ -n "$jwt" ] || fail "GitHub issued no OIDC token"
# The subject Azure will match, for the record: base64url → base64.
payload=$(printf '%s' "$jwt" | cut -d. -f2 | tr '_-' '/+')
pad=$(( (4 - ${#payload} % 4) % 4 )); while [ "$pad" -gt 0 ]; do payload="$payload="; pad=$((pad - 1)); done
printf '%s' "$payload" | base64 -d 2>/dev/null | jq -r '"signing-token: oidc subject \(.sub), audience \(.aud)"' >&2 || true

resp=$(curl -sS -K - <<CURL
url = "https://login.microsoftonline.com/${AZURE_TENANT_ID}/oauth2/v2.0/token"
data-urlencode = "client_id=${AZURE_CLIENT_ID}"
data-urlencode = "grant_type=client_credentials"
data-urlencode = "scope=https://codesigning.azure.net/.default"
data-urlencode = "client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
data-urlencode = "client_assertion=${jwt}"
CURL
)
token=$(printf '%s' "$resp" | jq -r '.access_token // empty' 2>/dev/null || true)
if [ -z "$token" ]; then
  show "$resp" '{error, error_description}'
  fail "Azure refused the exchange; the federated credential's subject must equal the oidc subject above, and the variables must name the right tenant and client"
fi
printf '%s' "$resp" | jq -r '"signing-token: an access token for https://codesigning.azure.net was issued (\(.token_type), \(.expires_in)s); it is on standard output only"' >&2
printf '%s' "$token"

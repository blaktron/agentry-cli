#!/usr/bin/env bash
# Authenticode-signs one PE file in place against Azure Artifact Signing, from
# Linux, with jsign (the desktop's #54, copied for agentry-cli#196; plan os-code-signing.md §4, D2, D6).
#
#   scripts/sign-windows.sh fetch <dir>     download jsign into <dir> and verify its digest
#   scripts/sign-windows.sh <file.exe>      sign <file.exe> in place, then verify the result
#
# Signing reads its inputs from the environment and nothing from argv, so the
# access token is in no process listing (D-026's standard for the
# update-signing key holds here too):
#
#   AGENTRY_SIGNING_TOKEN       an Azure access token for https://codesigning.azure.net
#                               (the release job exchanges its OIDC token for one)
#   ARTIFACT_SIGNING_ENDPOINT   e.g. eus.codesigning.azure.net
#   ARTIFACT_SIGNING_ACCOUNT    the Artifact Signing account name
#   ARTIFACT_SIGNING_PROFILE    the Public Trust certificate profile name
#   JSIGN_JAR                   the jar `fetch` wrote (defaults to $RUNNER_TEMP/jsign.jar)
#
# Microsoft's statement of the service is that the key never leaves its HSM
# and the certificate "is never given to you"; jsign sends the file's digest
# and gets the signature back. Artifact Signing's certificates live three days, so
# every signature is timestamped by Microsoft's own TSA or it dies with the
# certificate. The release workflow calls this script for the one Windows
# binary the CLI ships; nothing in the CLI's build or tests does (plan
# cli-release-signing.md D5).
set -euo pipefail

JSIGN_VERSION=7.5
JSIGN_SHA256=602a51c3545a6dc4fb99bd2ea7152b26d1345916d0c93ddfbd5936cb735af91c
JSIGN_URL="https://github.com/ebourg/jsign/releases/download/${JSIGN_VERSION}/jsign-${JSIGN_VERSION}.jar"
TSA_URL="http://timestamp.acs.microsoft.com"

fail() { echo "sign-windows: $*" >&2; exit 1; }

if [ "${1:-}" = "fetch" ]; then
  dir="${2:?usage: $0 fetch <dir>}"
  mkdir -p "$dir"
  jar="$dir/jsign.jar"
  if [ ! -f "$jar" ] || ! printf '%s  %s\n' "$JSIGN_SHA256" "$jar" | sha256sum -c --quiet - 2>/dev/null; then
    curl -sSfL --retry 3 -o "$jar.part" "$JSIGN_URL" || fail "could not download $JSIGN_URL"
    printf '%s  %s\n' "$JSIGN_SHA256" "$jar.part" | sha256sum -c --quiet - \
      || { rm -f "$jar.part"; fail "jsign-${JSIGN_VERSION}.jar does not have the pinned digest $JSIGN_SHA256"; }
    mv "$jar.part" "$jar"
  fi
  echo "sign-windows: jsign $JSIGN_VERSION at $jar (sha256 $JSIGN_SHA256)"
  exit 0
fi

file="${1:?usage: $0 <file.exe> | $0 fetch <dir>}"
[ -f "$file" ] || fail "$file does not exist"
command -v java >/dev/null 2>&1 || fail "java is needed to run jsign (apt-get install default-jre-headless)"
: "${AGENTRY_SIGNING_TOKEN:?no AGENTRY_SIGNING_TOKEN in the environment: the token exchange of the release job did not run}"
: "${ARTIFACT_SIGNING_ENDPOINT:?ARTIFACT_SIGNING_ENDPOINT is not set (a repository variable)}"
: "${ARTIFACT_SIGNING_ACCOUNT:?ARTIFACT_SIGNING_ACCOUNT is not set (a repository variable)}"
: "${ARTIFACT_SIGNING_PROFILE:?ARTIFACT_SIGNING_PROFILE is not set (a repository variable)}"
jar="${JSIGN_JAR:-${RUNNER_TEMP:-/tmp}/jsign.jar}"
[ -f "$jar" ] || fail "$jar is missing: run '$0 fetch $(dirname "$jar")' first"
printf '%s  %s\n' "$JSIGN_SHA256" "$jar" | sha256sum -c --quiet - 2>/dev/null || fail "$jar is not jsign $JSIGN_VERSION (digest mismatch)"

# --storepass env:NAME makes jsign read the token from the environment; the
# alias is <account>/<profile>; RFC 3161 is the timestamp protocol the
# Microsoft TSA speaks; the name and URL are what Windows shows beside the
# publisher in the signature's details.
java -jar "$jar" sign \
  --storetype TRUSTEDSIGNING \
  --keystore "$ARTIFACT_SIGNING_ENDPOINT" \
  --storepass env:AGENTRY_SIGNING_TOKEN \
  --alias "$ARTIFACT_SIGNING_ACCOUNT/$ARTIFACT_SIGNING_PROFILE" \
  --tsaurl "$TSA_URL" --tsmode RFC3161 --tsretries 5 --tsretrywait 15 \
  --name "Agentry Desktop" --url "https://secureagentry.net" \
  --replace \
  "$file" || fail "jsign could not sign $file"
# jsign 7.5 has no verify command; the read-back — chain to the pinned root,
# timestamp, subject — is check-windows-binary.sh's with osslsigncode.
echo "sign-windows: signed $(basename "$file") with $ARTIFACT_SIGNING_ACCOUNT/$ARTIFACT_SIGNING_PROFILE at $ARTIFACT_SIGNING_ENDPOINT, timestamped by $TSA_URL"

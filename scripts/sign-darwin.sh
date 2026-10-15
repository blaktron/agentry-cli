#!/usr/bin/env bash
# Developer ID-signs and notarizes bare darwin binaries in place, on a macOS
# runner.
#
#   scripts/sign-darwin.sh <binary>...
#
# For each binary: codesign under the hardened runtime with Apple's timestamp
# and the stable identifier net.secureagentry.cli; read the signature back
# (the identity and the Team ID release.env names, the runtime flag, a
# timestamp); submit a zip of it to Apple's notary service and wait for
# "Accepted"; then ask Gatekeeper for "Notarized Developer ID" the way it
# judges a bare binary (`spctl --assess --type open --context
# context:primary-signature`) and have codesign check Apple's ticket
# (`--check-notarization -R=notarized`). `spctl --type execute` is for app
# bundles and rejects every bare binary, notarized or not ("does not seem to
# be an app"). A bare Mach-O cannot be stapled, so the ticket is Apple's
# online one, which is what Gatekeeper looks up for a quarantined download.
#
# Inputs come from the environment, never argv (the release job's secrets):
#
#   APPLE_DEVELOPER_ID_P12           the .p12, base64 (certificate, key, Apple's G2 intermediate)
#   APPLE_DEVELOPER_ID_P12_PASSWORD  its password
#   APPLE_NOTARY_KEY_P8              the App Store Connect API key, PEM text
#   APPLE_NOTARY_KEY_ID              its Key ID
#   APPLE_NOTARY_ISSUER_ID           its Issuer ID
#   APPLE_SIGNING_IDENTITY           the identity the signature must carry (build/release.env)
#   APPLE_TEAM_ID                    its Team ID (build/release.env)
#   SIGN_DARWIN_REPORT               optional: a file to write team= and notarized= into
#   NOTARY_TIMEOUT_MINUTES           optional: how long to wait for Apple, default 45
#
# The certificate lives in a keychain this script creates in its own 0700
# directory and deletes on exit, with the .p12 and the .p8; nothing persists
# on the runner. The keychain is on the build user's search list only while
# codesign runs (codesign finds no identity otherwise); `security
# delete-keychain` takes its entry off again and leaves every other entry.
# Headless codesign also needs Apple's Developer ID G2 intermediate in the
# host's System keychain: without it codesign fails with
# errSecInternalComponent. Nothing in the CLI's build or tests calls this.
# macOS's bash is 3.2: nothing here needs bash 4.
set -euo pipefail

IDENTIFIER=net.secureagentry.cli
OPENSSL=/usr/bin/openssl

fail() { echo "sign-darwin: $*" >&2; exit 1; }

[ "$#" -ge 1 ] || fail "usage: $0 <binary>..."
[ "$(uname -s)" = Darwin ] || fail "this runs on macOS (codesign, notarytool, spctl)"
for v in APPLE_DEVELOPER_ID_P12 APPLE_DEVELOPER_ID_P12_PASSWORD APPLE_NOTARY_KEY_P8 APPLE_NOTARY_KEY_ID APPLE_NOTARY_ISSUER_ID APPLE_SIGNING_IDENTITY APPLE_TEAM_ID; do
  eval "val=\${$v:-}"
  [ -n "$val" ] || fail "$v is not set"
done
val=""
for f in "$@"; do [ -f "$f" ] || fail "$f is not a file"; done
for t in security codesign spctl ditto plutil base64; do command -v "$t" >/dev/null 2>&1 || fail "$t is missing"; done
xcrun --find notarytool >/dev/null 2>&1 || fail "xcrun cannot find notarytool (the Command Line Tools)"

W=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/sign-darwin.XXXXXX") || fail "no temporary directory"
chmod 700 "$W"
# The search list holds resolved paths (/private/tmp, not /tmp): compare
# against the same, or an entry is never recognised as this one.
W=$(cd "$W" && pwd -P)
KC="$W/signing.keychain-db"

# The user's keychain search list, one path per line.
kc_list() { security list-keychains -d user | sed -e 's/^[[:space:]]*"//' -e 's/"[[:space:]]*$//'; }
# Put KC first on the list, keeping whatever else is there.
kc_add() {
  local l; set -- "$KC"
  while IFS= read -r l; do [ -n "$l" ] && [ "$l" != "$KC" ] && set -- "$@" "$l"; done <<EOF
$(kc_list)
EOF
  security list-keychains -d user -s "$@"
}
# Deleting the keychain also takes it off the list (`list-keychains -s` with
# no argument changes nothing, so an emptied list cannot be written back).
cleanup() {
  security delete-keychain "$KC" >/dev/null 2>&1 || true
  rm -rf "$W"
  if kc_list | grep -qF "$(basename "$W")"; then echo "sign-darwin: the signing keychain is still on the search list" >&2; return 1; fi
  return 0
}
# A keychain left on the list fails the run.
trap 'cleanup || exit 1' EXIT

# The secrets, into the 0700 directory, never echoed.
( umask 077
  printf '%s' "$APPLE_DEVELOPER_ID_P12" | base64 --decode > "$W/id.p12"
  printf '%s\n' "$APPLE_NOTARY_KEY_P8" > "$W/notary.p8" )
grep -q 'BEGIN PRIVATE KEY' "$W/notary.p8" || fail "APPLE_NOTARY_KEY_P8 is not a PEM private key"

kcpass=$("$OPENSSL" rand -hex 24)
security create-keychain -p "$kcpass" "$KC" >/dev/null
security set-keychain-settings -lut 3600 "$KC"
security unlock-keychain -p "$kcpass" "$KC"
# -P puts the .p12 password on this one command line for the moment it runs;
# the runner's user runs nothing else of anyone's.
security import "$W/id.p12" -k "$KC" -P "$APPLE_DEVELOPER_ID_P12_PASSWORD" -f pkcs12 \
  -T /usr/bin/codesign >/dev/null || fail "security import refused the .p12 (the password?)"
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$kcpass" "$KC" >/dev/null
rm -f "$W/id.p12"
kcpass=""

kc_add
sha1=$(security find-identity -v -p codesigning "$KC" | awk -v id="\"$APPLE_SIGNING_IDENTITY\"" 'index($0, id) {print $2; exit}')
[ -n "$sha1" ] || { security find-identity -v -p codesigning "$KC" >&2; fail "no valid identity \"$APPLE_SIGNING_IDENTITY\" in the .p12 (is Apple's G2 intermediate in the System keychain?)"; }
echo "identity $APPLE_SIGNING_IDENTITY ($sha1)"

for f in "$@"; do
  # Another job on this user may have rewritten the list; put KC back first.
  kc_add
  codesign --force --options runtime --timestamp --identifier "$IDENTIFIER" \
    --keychain "$KC" --sign "$sha1" "$f" || fail "codesign failed on $f"
done
security delete-keychain "$KC"

# The read-back: every claim the release makes about the signature.
for f in "$@"; do
  codesign --verify --strict --verbose=2 "$f" || fail "codesign --verify --strict refused $f"
  d=$(codesign -dvv "$f" 2>&1)
  grep -qxF "Authority=$APPLE_SIGNING_IDENTITY" <<<"$d" || { echo "$d" >&2; fail "$f is not signed by $APPLE_SIGNING_IDENTITY"; }
  grep -qxF "TeamIdentifier=$APPLE_TEAM_ID" <<<"$d" || { echo "$d" >&2; fail "$f does not carry Team ID $APPLE_TEAM_ID"; }
  grep -qxF "Identifier=$IDENTIFIER" <<<"$d" || { echo "$d" >&2; fail "$f does not carry the identifier $IDENTIFIER"; }
  grep -q '^Timestamp=' <<<"$d" || { echo "$d" >&2; fail "$f has no secure timestamp"; }
  grep -q '^CodeDirectory .*flags=.*(runtime)' <<<"$d" || { echo "$d" >&2; fail "$f is not signed under the hardened runtime"; }
  echo "signed $(basename "$f"): $(grep -E '^(Timestamp|TeamIdentifier)=' <<<"$d" | tr '\n' ' ')"
done

# Notarization: a zip per binary, all submitted first so Apple works on them
# together, then each polled until it is decided. A network error while
# polling is retried until the deadline; anything but Accepted prints Apple's
# log and fails.
notary() { xcrun notarytool "$@" --key "$W/notary.p8" --key-id "$APPLE_NOTARY_KEY_ID" --issuer "$APPLE_NOTARY_ISSUER_ID"; }
ids=""
for f in "$@"; do
  z="$W/$(basename "$f").zip"
  ditto -c -k --keepParent "$f" "$z"
  notary submit "$z" --output-format json > "$W/submit.json" 2> "$W/submit.err" || { cat "$W/submit.err" >&2; fail "notarytool could not submit $(basename "$f")"; }
  id=$(plutil -extract id raw -o - "$W/submit.json" 2>/dev/null || true)
  [ -n "$id" ] || { cat "$W/submit.json" "$W/submit.err" >&2; fail "notarytool returned no submission id for $(basename "$f")"; }
  echo "submitted $(basename "$f") for notarization: $id"
  ids="$ids $id"
  rm -f "$z"
done
deadline=$(( $(date +%s) + ${NOTARY_TIMEOUT_MINUTES:-45} * 60 ))
for id in $ids; do
  while :; do
    status=""
    if notary info "$id" --output-format json > "$W/info.json" 2> "$W/info.err"; then
      status=$(plutil -extract status raw -o - "$W/info.json" 2>/dev/null || true)
    else
      echo "notarytool info $id failed (retrying): $(head -c 200 "$W/info.err")"
    fi
    case "$status" in
      Accepted) echo "notarization $id: Accepted"; break ;;
      Invalid|Rejected) notary log "$id" >&2 || true; fail "notarization $id: $status" ;;
    esac
    [ "$(date +%s)" -lt "$deadline" ] || fail "notarization $id still ${status:-unknown} after ${NOTARY_TIMEOUT_MINUTES:-45} minutes"
    sleep 30
  done
done
rm -f "$W/notary.p8"

# Gatekeeper's verdict, from Apple's online ticket; it can take a moment to
# be served after "Accepted". An unnotarized control binary is "rejected,
# source=Unnotarized Developer ID" and fails the requirement.
for f in "$@"; do
  ok=""
  for i in 1 2 3 4 5 6; do
    out=$(spctl --assess --type open --context context:primary-signature --verbose=2 "$f" 2>&1 || true)
    if grep -q ': accepted$' <<<"$out" && grep -qxF 'source=Notarized Developer ID' <<<"$out" \
      && codesign --verify --strict --check-notarization -R="notarized" "$f" 2>/dev/null; then ok=1; break; fi
    sleep 20
  done
  echo "$out"
  [ -n "$ok" ] || { codesign --verify --strict --check-notarization -R="notarized" -vvv "$f" >&2 || true; fail "Gatekeeper does not accept $(basename "$f") as Notarized Developer ID"; }
done

if [ -n "${SIGN_DARWIN_REPORT:-}" ]; then
  { echo "team=$APPLE_TEAM_ID"; echo "notarized=true"; echo "submissions=${ids# }"; } > "$SIGN_DARWIN_REPORT"
fi
echo "sign-darwin: $# binaries signed, notarized and accepted by Gatekeeper"

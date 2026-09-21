#!/usr/bin/env bash
# Acceptance for the CLI's Windows signing seam (agentry-cli#196): proves that
# check-windows-binary.sh accepts a signed, timestamped binary that chains to
# the root it is given and refuses the three ways it can be wrong — with a
# throwaway CA and osslsigncode standing in for Artifact Signing and jsign,
# so it runs anywhere osslsigncode does and needs no credential. The real
# key is only ever used by the release workflow. Nothing in the CLI's build
# or tests depends on this (plan D5); it is the release seam's own test.
#
#   scripts/acceptance/windows-signing.sh <agentry-<version>-windows-amd64.exe>
#
# Needs osslsigncode, openssl and network for a public timestamp authority
# (the checker insists on a timestamp, as the release must). On a workstation
# without osslsigncode, run it in a container:
#
#   docker run --rm --security-opt label=disable -v "$PWD:/w" -w /w ubuntu:24.04 bash -c \
#     'apt-get update -qq && apt-get install -y -qq --no-install-recommends osslsigncode openssl ca-certificates >/dev/null && scripts/acceptance/windows-signing.sh dist/agentry-0.0.0-windows-amd64.exe'
set -euo pipefail

exe="${1:?usage: $0 <agentry-<version>-windows-amd64.exe>}"
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { echo "windows-signing acceptance: $*" >&2; exit 1; }
[ -f "$exe" ] || fail "$exe does not exist"
for t in osslsigncode openssl; do command -v "$t" >/dev/null 2>&1 || fail "$t is needed"; done
unset AGENTRY_WINDOWS_SIGNING_CN SIGNING_CAFILE SIGNING_TSA_CAFILE

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
name=$(basename "$exe")
[[ "$name" =~ ^agentry-.+-windows-amd64\.exe$ ]] || name="agentry-0.0.0-windows-amd64.exe"

# A root and a leaf with the code-signing EKU, two days of life.
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$T/ca.key" -out "$T/ca.pem" -days 2 -subj "/CN=Agentry Test Root" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$T/leaf.key" -out "$T/leaf.csr" -subj "/C=CA/CN=Agentry Test Signer" 2>/dev/null
printf 'extendedKeyUsage=codeSigning\nkeyUsage=digitalSignature\nbasicConstraints=CA:FALSE\n' > "$T/leaf.ext"
openssl x509 -req -in "$T/leaf.csr" -CA "$T/ca.pem" -CAkey "$T/ca.key" -CAcreateserial -out "$T/leaf.pem" -days 2 -extfile "$T/leaf.ext" 2>/dev/null

# The stand-in for scripts/sign-windows.sh: the same contract, a public TSA
# for the timestamp, two tried in turn.
sign() {
  local f=$1 tsa
  for tsa in http://timestamp.digicert.com http://timestamp.sectigo.com; do
    if osslsigncode sign -certs "$T/leaf.pem" -key "$T/leaf.key" -n "Agentry" -i https://secureagentry.net -ts "$tsa" -in "$f" -out "$f.signed" >/dev/null 2>&1; then
      mv "$f.signed" "$f"; echo "test-sign: signed $(basename "$f") (timestamp $tsa)"; return 0
    fi
  done
  fail "no timestamp authority answered"
}

# refused <expected text> <args…>: the checker must fail AND say why.
refused() {
  local want=$1; shift
  local out
  if out=$("$root_dir/scripts/check-windows-binary.sh" "$@" 2>&1); then
    printf '%s\n' "$out"; fail "accepted what it must refuse: $want"
  fi
  printf '%s\n' "$out" | grep -q -F "$want" || { printf '%s\n' "$out"; fail "refused for another reason than '$want'"; }
}

cp "$exe" "$T/$name"
sign "$T/$name"

# 1. Accepted, and the report names the signer.
export SIGNING_CAFILE="$T/ca.pem" SIGNING_TSA_CAFILE=/etc/ssl/certs/ca-certificates.crt
"$root_dir/scripts/check-windows-binary.sh" --report "$T/report.txt" "$T/$name"
grep -q '^cn=Agentry Test Signer$' "$T/report.txt" || { cat "$T/report.txt"; fail "the report does not name the signer"; }
grep -q '^timestamp=' "$T/report.txt" || fail "the report carries no timestamp"

# 2. Refused: the unsigned binary.
mkdir -p "$T/unsigned"; cp "$exe" "$T/unsigned/$name"
refused "does not verify to" "$T/unsigned/$name"
echo "windows-signing acceptance: an unsigned binary refused"

# 3. Refused: a chain to a root that is not the one given.
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$T/other.key" -out "$T/other.pem" -days 2 -subj "/CN=Other Root" 2>/dev/null
SIGNING_CAFILE="$T/other.pem" refused "does not verify to" "$T/$name"
echo "windows-signing acceptance: a chain to another root refused"

# 4. Refused: a signer that names another CN than the release expects.
AGENTRY_WINDOWS_SIGNING_CN="Somebody Else" refused "expects 'Somebody Else'" "$T/$name"
echo "windows-signing acceptance: a CN other than the expected one refused"

# 5. The committed root is the one the checker pins.
unset SIGNING_CAFILE SIGNING_TSA_CAFILE
fp=$(openssl x509 -in "$root_dir/build/windows/signing/microsoft-identity-verification-root-2020.pem" -noout -fingerprint -sha256 | sed 's/^.*=//')
pinned=$(sed -n 's/^pinned_fp="\(.*\)"$/\1/p' "$root_dir/scripts/check-windows-binary.sh" | head -1)
[ -n "$pinned" ] && [ "$fp" = "$pinned" ] || fail "the committed Microsoft root has fingerprint $fp, the checker pins ${pinned:-nothing}"

echo "windows-signing acceptance: ok — a signed binary accepted and reported; three refusals proven; the pinned root is the one the checker pins"

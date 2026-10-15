#!/usr/bin/env bash
# Reads a Windows CLI binary back and checks its Authenticode signature:
#
#   scripts/check-windows-binary.sh [--report FILE] <agentry-<version>-windows-amd64.exe>
#
# What it proves:
#   1. the file name is one the site's manifest admits;
#   2. it carries exactly ONE Authenticode signature that osslsigncode verifies
#      to the pinned Microsoft Identity Verification root
#      (build/windows/signing/), timestamped and the timestamp verified;
#   3. the signer's CN is AGENTRY_WINDOWS_SIGNING_CN when that is in the
#      environment (the release exports it from build/release.env) — the
#      pinned root is every Artifact Signing customer's, so the name is what
#      says the signature is ours.
# --report FILE writes what was found (cn=, timestamp=, root=) for BUILD-INFO.
# Needs osslsigncode 2.7 or later (both spellings of its timestamp line are
# read). Overrides for the test harness only: SIGNING_CAFILE (the root to
# verify against; the fingerprint pin is then skipped) and SIGNING_TSA_CAFILE
# (the timestamp chain's root, default: the same file).
#
# Nothing in the CLI's build or tests needs this script or anything it needs:
# it runs in the release workflow and in the seam acceptance only.
set -euo pipefail

report=""
while [ $# -gt 0 ]; do
  case "$1" in
    --report) report="${2:?--report needs a file}"; shift 2 ;;
    --) shift; break ;;
    -*) echo "check-windows-binary: unknown option $1" >&2; exit 2 ;;
    *) break ;;
  esac
done
exe="${1:?usage: $0 [--report FILE] <agentry-<version>-windows-amd64.exe>}"
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail() { echo "check-windows-binary: $*" >&2; exit 1; }

[ -f "$exe" ] || fail "$exe does not exist"
command -v osslsigncode >/dev/null 2>&1 || fail "osslsigncode is needed to verify the signature (apt-get install osslsigncode)"
command -v openssl >/dev/null 2>&1 || fail "openssl is needed to check the pinned root"

# 1. The name.
name=$(basename "$exe")
[[ "$name" =~ ^agentry-.+-windows-amd64\.exe$ ]] || fail "$name is not agentry-<version>-windows-amd64.exe, which the download manifest would drop"

# 2. The pinned root, checked before it is trusted with anything.
pinned_root="$root_dir/build/windows/signing/microsoft-identity-verification-root-2020.pem"
pinned_fp="53:67:F2:0C:7A:DE:0E:2B:CA:79:09:15:05:6D:08:6B:72:0C:33:C1:FA:2A:26:61:AC:F7:87:E3:29:2E:12:70"
ca="${SIGNING_CAFILE:-$pinned_root}"
tsa_ca="${SIGNING_TSA_CAFILE:-$ca}"
[ -f "$ca" ] || fail "$ca does not exist"
if [ -z "${SIGNING_CAFILE:-}" ]; then
  fp=$(openssl x509 -in "$ca" -noout -fingerprint -sha256 | sed 's/^.*=//')
  [ "$fp" = "$pinned_fp" ] || fail "$ca is not the pinned Microsoft Identity Verification root (fingerprint $fp)"
fi
root_subject=$(openssl x509 -in "$ca" -noout -subject | sed 's/^subject=//')

if ! out=$(osslsigncode verify -in "$exe" -CAfile "$ca" -TSA-CAfile "$tsa_ca" 2>&1); then
  printf '%s\n' "$out" >&2
  fail "the signature on $name does not verify to $root_subject"
fi
# osslsigncode exits 0 when at least one signature verifies and a PE can carry
# several (nested); a release carries exactly one, so exactly one is accepted.
printf '%s\n' "$out" | grep -q -x 'Number of verified signatures: 1' || { printf '%s\n' "$out" >&2; fail "$name does not carry exactly one verified signature"; }
if printf '%s\n' "$out" | grep -q 'Nested Signature'; then printf '%s\n' "$out" >&2; fail "$name carries a nested signature"; fi
printf '%s\n' "$out" | grep -q -x 'Signature verification: ok' || { printf '%s\n' "$out" >&2; fail "osslsigncode did not report the signature on $name ok"; }
# The signer's CN, in either subject form osslsigncode prints: 2.8's OpenSSL
# slash form (/C=CA/…/CN=Name) and 2.12's RFC 2253 comma form
# (CN=Name,O=…,C=CA; a comma inside a value is escaped with a backslash and a
# component boundary is an unescaped comma). No CN in the form it uses is a
# refusal, never a guess from the other form.
subject=$(printf '%s\n' "$out" | awk '/Signer.s certificate/{on=1} on && /Subject:/{sub(/^[[:space:]]*Subject:[[:space:]]*/, ""); print; exit}')
case "$subject" in
  /*) cn=$(printf '%s\n' "$subject" | sed -n 's#.*/CN=\([^/]*\).*#\1#p') ;;
  *)  cn=$(printf '%s\n' "$subject" | sed -E -n 's/^(.*[^\\],)?[[:space:]]*CN=((\\.|[^,])+).*$/\2/p' | sed -E 's/\\(.)/\1/g') ;;
esac
cn=$(printf '%s' "$cn" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
[ -n "$cn" ] || { printf '%s\n' "$out" >&2; fail "could not read the signer's CN from osslsigncode's report on $name (subject: ${subject:-none})"; }
ts=$(printf '%s\n' "$out" | sed -n -e 's/^[[:space:]]*Timestamp time: //p' -e 's/^The signature is timestamped: //p' | head -1)
[ -n "$ts" ] || { printf '%s\n' "$out" >&2; fail "the signature on $name is not timestamped — an Artifact Signing certificate lives three days, so the signature would die with it"; }
printf '%s\n' "$out" | grep -q -x 'Timestamp Server Signature verification: ok' || { printf '%s\n' "$out" >&2; fail "the timestamp on $name does not verify"; }

# 3. The CN the release expects, when it names one.
expected_cn="${AGENTRY_WINDOWS_SIGNING_CN:-}"
if [ -n "$expected_cn" ] && [ "$expected_cn" != "$cn" ]; then
  fail "$name is signed by CN='$cn' but AGENTRY_WINDOWS_SIGNING_CN expects '$expected_cn'"
fi

if [ -n "$report" ]; then
  { printf 'cn=%s\n' "$cn"; printf 'timestamp=%s\n' "$ts"; printf 'root=%s\n' "$root_subject"; } > "$report"
fi
echo "check-windows-binary: $name ok — signed by CN=$cn, one signature, timestamped $ts, chain to $root_subject"

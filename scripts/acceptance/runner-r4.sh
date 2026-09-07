#!/usr/bin/env bash
# M-R4a acceptance (plan runner-m-r4-adapters §6, RUNNER-DEVIATIONS DR-6):
# the proxy evidence on docker-agent. A run whose policy allowlists the
# model host succeeds and receipts/proxy.log shows the CONNECT allowed; a
# run whose policy omits it fails — classified, within the timeout — and
# proxy.log shows the CONNECT denied and none allowed. Gates as runner-r2.sh:
# a docker daemon, the operator policy and the model key it names.
set -euo pipefail
cd "$(dirname "$0")/../.."
AGENTRY=${AGENTRY:-bin/agentry}
[ -x "$AGENTRY" ] || make build >/dev/null
if ! docker info >/dev/null 2>&1; then
  echo "SKIP: no reachable docker daemon (RUNNER-DEVIATIONS DR-7)" >&2
  exit 0
fi
POLICY=${AGENTRY_POLICY:-$HOME/.agentry/runner/policy.yaml}
[ -f "$POLICY" ] || { echo "SKIP: no runner policy at $POLICY (the allowlist is model.proxyAllow)" >&2; exit 0; }
KEY_ENV=$(awk '/^ *apiKeyEnv:/ {print $2; exit}' "$POLICY")
KEY_ENV=${KEY_ENV:-ANTHROPIC_API_KEY}
if [ -z "${!KEY_ENV:-}" ]; then
  echo "SKIP: $KEY_ENV not set — the agent container needs a model" >&2
  exit 0
fi
# The model host:port the proxy sees: model.baseUrl, else Anthropic's API.
BASE=$(awk '/^ *baseUrl:/ {print $2; exit}' "$POLICY")
HOSTPORT=$(python3 - "${BASE:-https://api.anthropic.com}" <<'PY'
import sys, urllib.parse
u = urllib.parse.urlparse(sys.argv[1])
print(f"{u.hostname}:{u.port or (443 if u.scheme == 'https' else 80)}")
PY
)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export AGENTRY_RUNNER_HOME="$WORK/home"
# The same policy with an allowlist that names no real host.
python3 - "$POLICY" "$WORK/denied.yaml" <<'PY'
import re, sys
src, dst = sys.argv[1], sys.argv[2]
out, skipping = [], False
for line in open(src).read().splitlines():
    if re.match(r"^\s*proxyAllow:\s*\[", line):
        out.append(re.sub(r"\[.*\]", "[denied.invalid:443]", line)); continue
    if re.match(r"^\s*proxyAllow:\s*$", line):
        out.append(line); out.append("    - denied.invalid:443"); skipping = True; continue
    if skipping:
        if re.match(r"^\s*-\s", line):
            continue
        skipping = False
    out.append(line)
open(dst, "w").write("\n".join(out) + "\n")
PY
"$AGENTRY" run --local testdata/runner/hello-report --allow-unsigned --yes --policy "$POLICY" --prepare
echo "prepare ok"

OUT_OK="$WORK/ok"
echo "please make the report" | "$AGENTRY" run --local testdata/runner/hello-report --policy "$POLICY" \
  --allow-unsigned --yes --out "$OUT_OK" --output-format json --timeout 300 >"$WORK/ok.json"
grep -q '"status": "ok"' "$WORK/ok.json" || { echo "FAIL: allowed run not ok"; cat "$WORK/ok.json"; exit 1; }
[ -f "$OUT_OK/receipts/proxy.log" ] || { echo "FAIL: proxy.log receipt missing"; exit 1; }
grep -q "allow $HOSTPORT" "$OUT_OK/receipts/proxy.log" || { echo "FAIL: proxy.log shows no allowed CONNECT to $HOSTPORT"; cat "$OUT_OK/receipts/proxy.log"; exit 1; }
echo "allowed run: CONNECT to $HOSTPORT allowed; evidence in receipts/proxy.log"

OUT_DENIED="$WORK/denied"
set +e
echo "please make the report" | "$AGENTRY" run --local testdata/runner/hello-report --policy "$WORK/denied.yaml" \
  --allow-unsigned --yes --out "$OUT_DENIED" --output-format json --timeout 120 >"$WORK/denied.json" 2>"$WORK/denied.err"
CODE=$?
set -e
[ "$CODE" -ne 0 ] || { echo "FAIL: the run with the model host denied succeeded"; cat "$WORK/denied.json"; exit 1; }
[ -f "$OUT_DENIED/receipts/proxy.log" ] || { echo "FAIL: proxy.log receipt missing on the denied run"; tail -20 "$WORK/denied.err"; exit 1; }
grep -q "deny $HOSTPORT" "$OUT_DENIED/receipts/proxy.log" || { echo "FAIL: proxy.log shows no denied CONNECT to $HOSTPORT"; cat "$OUT_DENIED/receipts/proxy.log"; tail -20 "$WORK/denied.err"; exit 1; }
if grep -q "allow $HOSTPORT" "$OUT_DENIED/receipts/proxy.log"; then echo "FAIL: the denied policy still allowed $HOSTPORT"; exit 1; fi
echo "denied run: exit $CODE, CONNECT to $HOSTPORT denied; evidence in receipts/proxy.log"

LEFT=$(docker ps -a --filter label=agentry.run --format '{{.Names}}')
[ -z "$LEFT" ] || { echo "FAIL: leftover containers: $LEFT"; exit 1; }
echo "runner-r4: proxy evidence for docker-agent passed"

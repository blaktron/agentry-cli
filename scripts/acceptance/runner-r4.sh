#!/usr/bin/env bash
# Adapter acceptance: per-adapter proxy evidence, and the envelope diff that
# says every adapter runs the same QuickAgent the same way.
#
#   scripts/acceptance/runner-r4.sh [adapter...]   (default: docker-agent)
#
# Per adapter, two runs of one fixture QuickAgent:
#   1. the policy allowlists the model host — the run succeeds and
#      receipts/proxy.log shows the CONNECT allowed;
#   2. the policy does not — the run ends classified (never hangs: the
#      budgets apply) and proxy.log shows the CONNECT denied and none
#      allowed. For a harness that runs as a host process this is the run
#      that matters: it has a direct route to the model, so failing here is
#      what proves it honours the proxy variables (a harness that ignores
#      them is not shipped).
# With more than one adapter it also diffs the result envelopes with the
# model-dependent fields masked (conformance diffing).
#
# Gates as runner-r2.sh: a docker daemon, the operator policy, and the key
# the policy names. An adapter whose harness is not installed is skipped.
#
# This covers autonomous runs. The interactive half is
# runner-r4-session.py, which needs a pty and so runs on its own.
set -euo pipefail
cd "$(dirname "$0")/../.."
AGENTRY=${AGENTRY:-bin/agentry}
[ -x "$AGENTRY" ] || make build >/dev/null
ADAPTERS=("$@"); [ ${#ADAPTERS[@]} -gt 0 ] || ADAPTERS=(docker-agent)
if ! docker info >/dev/null 2>&1; then
  echo "SKIP: no reachable docker daemon" >&2
  exit 0
fi
POLICY=${AGENTRY_POLICY:-$HOME/.agentry/runner/policy.yaml}
[ -f "$POLICY" ] || { echo "SKIP: no runner policy at $POLICY (the allowlist is model.proxyAllow)" >&2; exit 0; }
KEY_ENV=$(awk '/^ *apiKeyEnv:/ {print $2; exit}' "$POLICY")
KEY_ENV=${KEY_ENV:-ANTHROPIC_API_KEY}
if [ -z "${!KEY_ENV:-}" ]; then
  echo "SKIP: $KEY_ENV not set — the harness needs a model" >&2
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
# The consent records `agentry run` writes go to the throwaway config dir,
# not the person's ~/.config/agentry/installed.yaml.
export AGENTRY_CONFIG_DIR="$WORK/config"
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

# harness_missing says an adapter needs an installed harness this machine
# does not have, so the suite skips it rather than failing on it.
harness_missing() {
  "$AGENTRY" doctor --adapter "$1" --policy "$POLICY" --json 2>/dev/null |
    python3 -c '
import json,sys
try: rep=json.load(sys.stdin)
except Exception: sys.exit(1)
sys.exit(0 if any(c["name"].startswith("harness:") and c["status"]=="fail" for c in rep["checks"]) else 1)'
}

RAN=()
for A in "${ADAPTERS[@]}"; do
  if harness_missing "$A"; then
    echo "SKIP $A: its harness is not installed on this machine (agentry doctor --adapter $A)" >&2
    continue
  fi
  echo "== $A =="
  "$AGENTRY" run --local testdata/runner/hello-report --adapter "$A" \
    --allow-unsigned --yes --policy "$POLICY" --prepare
  echo "$A: prepare ok"

  OUT_OK="$WORK/$A/ok"
  echo "please make the report" | "$AGENTRY" run --local testdata/runner/hello-report --adapter "$A" \
    --policy "$POLICY" --allow-unsigned --yes --out "$OUT_OK" --output-format json --timeout 300 >"$WORK/$A-ok.json"
  grep -q '"status": "ok"' "$WORK/$A-ok.json" || { echo "FAIL $A: allowed run not ok"; cat "$WORK/$A-ok.json"; exit 1; }
  [ -f "$OUT_OK/receipts/proxy.log" ] || { echo "FAIL $A: proxy.log receipt missing"; exit 1; }
  grep -q "allow $HOSTPORT" "$OUT_OK/receipts/proxy.log" || {
    echo "FAIL $A: proxy.log shows no allowed CONNECT to $HOSTPORT"; cat "$OUT_OK/receipts/proxy.log"; exit 1; }
  # A run can exit "ok" having accomplished nothing if its tool calls were
  # refused (a harness whose MCP approval was misconfigured does exactly
  # that). The hello-report agent's whole job is to read notes.txt and
  # reply with its text, so the result naming the note proves the gateway
  # tools actually ran — the check that would have caught it.
  if ! grep -qi "hello from the workspace" "$WORK/$A-ok.json"; then
    echo "FAIL $A: the allowed run exited ok but its result does not contain the note — the gateway tools did not run"
    python3 -c "import json;print(json.load(open('$OUT_OK/result.json'))['result'][:400])" 2>/dev/null
    exit 1
  fi
  echo "$A allowed run: CONNECT to $HOSTPORT allowed and the agent used the gateway (result names the note); evidence in receipts/proxy.log"

  OUT_DENIED="$WORK/$A/denied"
  set +e
  echo "please make the report" | "$AGENTRY" run --local testdata/runner/hello-report --adapter "$A" \
    --policy "$WORK/denied.yaml" --allow-unsigned --yes --out "$OUT_DENIED" --output-format json \
    --timeout 120 >"$WORK/$A-denied.json" 2>"$WORK/$A-denied.err"
  CODE=$?
  set -e
  [ "$CODE" -ne 0 ] || { echo "FAIL $A: the run with the model host denied succeeded"; cat "$WORK/$A-denied.json"; exit 1; }
  [ -f "$OUT_DENIED/receipts/proxy.log" ] || { echo "FAIL $A: proxy.log missing on the denied run"; tail -20 "$WORK/$A-denied.err"; exit 1; }
  grep -q "deny $HOSTPORT" "$OUT_DENIED/receipts/proxy.log" || {
    echo "FAIL $A: proxy.log shows no denied CONNECT to $HOSTPORT — the harness did not even try the proxy"
    cat "$OUT_DENIED/receipts/proxy.log"; tail -20 "$WORK/$A-denied.err"; exit 1; }
  if grep -q "allow $HOSTPORT" "$OUT_DENIED/receipts/proxy.log"; then
    echo "FAIL $A: the denied policy still allowed $HOSTPORT"; exit 1; fi
  echo "$A denied run: exit $CODE, CONNECT to $HOSTPORT denied; evidence in receipts/proxy.log"
  # AGENTRY_R4_EVIDENCE keeps the allowed run's receipts, which is how the
  # conformance fixtures under pkg/runner/adapter/testdata are re-recorded
  # when a harness release changes.
  if [ -n "${AGENTRY_R4_EVIDENCE:-}" ]; then
    mkdir -p "$AGENTRY_R4_EVIDENCE/$A"
    cp -r "$OUT_OK/receipts" "$AGENTRY_R4_EVIDENCE/$A/" 2>/dev/null || true
    cp "$WORK/$A-ok.json" "$AGENTRY_R4_EVIDENCE/$A/result.json" 2>/dev/null || true
    cp "$OUT_DENIED/receipts/proxy.log" "$AGENTRY_R4_EVIDENCE/$A/proxy-denied.log" 2>/dev/null || true
  fi
  RAN+=("$A")
done

# Conformance diffing: the same QuickAgent through every adapter must
# produce the same envelope once the model-dependent parts are masked.
if [ ${#RAN[@]} -gt 1 ]; then
  for A in "${RAN[@]}"; do
    python3 - "$WORK/$A/ok/result.json" "$WORK/$A.masked" <<'PY'
import json, sys
env = json.load(open(sys.argv[1]))
# What legitimately differs between harnesses: which model said what, how
# long it took, and the run's identity. Everything else is the contract.
for k in ("runId", "usage", "result", "harness", "receipts", "outDir", "startedAt", "endedAt"):
    env.pop(k, None)
# One enforcement claim is REQUIRED to differ: a harness that runs as a
# host process says so, because the run network is not what bounds it. The
# receipt would be dishonest if it read like a container's. Every other
# capability claim — egress, filesystem, exec, secrets — must match, which
# is what this diff is for.
env.get("capabilities_enforced", {}).pop("harness.isolation", None)
json.dump(env, open(sys.argv[2], "w"), indent=2, sort_keys=True)
PY
  done
  BASE_A=${RAN[0]}
  for A in "${RAN[@]:1}"; do
    if ! diff -u "$WORK/$BASE_A.masked" "$WORK/$A.masked"; then
      echo "FAIL: $A's envelope differs from $BASE_A's outside the masked fields"; exit 1
    fi
  done
  echo "envelope diff: ${RAN[*]} agree once model-dependent fields are masked"
fi

LEFT=$(docker ps -a --filter label=agentry.run --format '{{.Names}}')
[ -z "$LEFT" ] || { echo "FAIL: leftover containers: $LEFT"; exit 1; }
echo "runner-r4: proxy evidence passed for ${RAN[*]:-nothing}"

#!/usr/bin/env bash
# M-R2 acceptance (RUNNER-PLAN §R12): the real execute path on a plain
# docker engine — first-use image install, gateway + tools + proxy + agent
# containers, result envelope, receipts, and the no-leak teardown sweep.
#
# Requirements (the script gates on each and says so):
#   - a reachable docker daemon (docker CE is enough; no Desktop features)
#   - ANTHROPIC_API_KEY in the environment (or edit the policy below)
#   - network access to pull the pinned images on first use
set -euo pipefail
cd "$(dirname "$0")/../.."

AGENTRY=${AGENTRY:-bin/agentry}
[ -x "$AGENTRY" ] || make build >/dev/null

if ! docker info >/dev/null 2>&1; then
  echo "SKIP: no reachable docker daemon (RUNNER-DEVIATIONS DR-7)" >&2
  exit 0
fi
if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  echo "SKIP: ANTHROPIC_API_KEY not set — the agent container needs a model" >&2
  exit 0
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export AGENTRY_RUNNER_HOME="$WORK/home"
OUT="$WORK/out"

# --prepare: verify + compile + pull/build images, no execution.
"$AGENTRY" run --local testdata/runner/hello-report --allow-unsigned --yes --prepare
echo "prepare ok"

# The run itself: task on stdin, envelope on stdout.
echo "please make the report" | "$AGENTRY" run --local testdata/runner/hello-report \
  --allow-unsigned --yes --out "$OUT" --output-format json --timeout 300 >"$WORK/envelope.json"

grep -q '"status": "ok"' "$WORK/envelope.json" || { echo "FAIL: run not ok"; cat "$WORK/envelope.json"; exit 1; }
grep -q '"runPlanSha256"' "$WORK/envelope.json" || { echo "FAIL: envelope lacks plan hash"; exit 1; }
[ -f "$OUT/result.json" ] || { echo "FAIL: result.json missing"; exit 1; }
[ -f "$OUT/receipts/events.ndjson" ] || { echo "FAIL: events receipt missing"; exit 1; }
[ -f "$OUT/receipts/run-plan.json" ] || { echo "FAIL: plan receipt missing"; exit 1; }
if [ ! -s "$OUT/reports/hello.md" ]; then
  echo "WARN: agent did not write reports/hello.md (model behavior — inspect receipts)" >&2
fi
echo "envelope + receipts ok"

# §R11 invariant 6: nothing of ours survives.
LEFT=$(docker ps -a --filter label=agentry.run --format '{{.Names}}')
[ -z "$LEFT" ] || { echo "FAIL: leftover containers: $LEFT"; exit 1; }
NETS=$(docker network ls --format '{{.Name}}' | grep '^agentry-' || true)
[ -z "$NETS" ] || { echo "FAIL: leftover networks: $NETS"; exit 1; }
find "$AGENTRY_RUNNER_HOME" -name secrets.env | grep -q . && { echo "FAIL: secrets.env survived"; exit 1; }
echo "teardown clean"

echo "runner-r2: all checks passed"

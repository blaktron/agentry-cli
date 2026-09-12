#!/usr/bin/env bash
# M-R2 acceptance (RUNNER-PLAN §R12): the real execute path on a plain
# docker engine — first-use image install, gateway + tools + proxy + agent
# containers, result envelope, receipts, and the no-leak teardown sweep.
#
# Requirements (the script gates on each and says so):
#   - a reachable docker daemon (docker CE is enough; no Desktop features)
#   - the model key named by the operator policy (~/.agentry/runner/policy.yaml,
#     apiKeyEnv) in the environment; ANTHROPIC_API_KEY when there is no policy
#   - network access to pull the pinned images on first use
set -euo pipefail
cd "$(dirname "$0")/../.."

AGENTRY=${AGENTRY:-bin/agentry}
[ -x "$AGENTRY" ] || make build >/dev/null

if ! docker info >/dev/null 2>&1; then
  echo "SKIP: no reachable docker daemon (RUNNER-DEVIATIONS DR-7)" >&2
  exit 0
fi
# The model comes from the operator policy when one exists (agentry-cli#56):
# its apiKeyEnv names the variable that must be set. Without a policy the
# runner's built-in default (anthropic, ANTHROPIC_API_KEY) applies as before.
POLICY=${AGENTRY_POLICY:-$HOME/.agentry/runner/policy.yaml}
POLICY_FLAG=()
KEY_ENV=ANTHROPIC_API_KEY
if [ -f "$POLICY" ]; then
  POLICY_FLAG=(--policy "$POLICY")
  KEY_ENV=$(awk '/^ *apiKeyEnv:/ {print $2; exit}' "$POLICY")
  KEY_ENV=${KEY_ENV:-ANTHROPIC_API_KEY}
fi
if [ -z "${!KEY_ENV:-}" ]; then
  echo "SKIP: $KEY_ENV not set — the agent container needs a model (policy: ${POLICY_FLAG[1]:-runner default})" >&2
  exit 0
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export AGENTRY_RUNNER_HOME="$WORK/home"
# The consent records `agentry run` writes (§R1) go to the throwaway config
# dir, not the person's ~/.config/agentry/installed.yaml (agentry-cli#101).
export AGENTRY_CONFIG_DIR="$WORK/config"
OUT="$WORK/out"

# --prepare: verify + compile + pull/build images, no execution.
"$AGENTRY" run --local testdata/runner/hello-report --allow-unsigned --yes ${POLICY_FLAG[@]+"${POLICY_FLAG[@]}"} --prepare
echo "prepare ok"

# The run itself: task on stdin, envelope on stdout.
echo "please make the report" | "$AGENTRY" run --local testdata/runner/hello-report ${POLICY_FLAG[@]+"${POLICY_FLAG[@]}"} \
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

# The exec catalog (agentry-cli#100): a BriefAgent declaring exec [git]
# runs in an exec image built from the catalogued package, and the receipt
# names the packages the build resolved. The model's use of git is not
# asserted (its tool-call shape varies; agentry-cli#113) — the build, the
# record and the receipt line are.
EXECPOL="$WORK/policy-exec.yaml"
if [ -f "$POLICY" ]; then cp "$POLICY" "$EXECPOL"; else "$AGENTRY" run --local testdata/runner/hello-report --allow-unsigned --yes --prepare >/dev/null 2>&1; cp "$HOME/.agentry/runner/policy.yaml" "$EXECPOL"; fi
if ! grep -q '^exec:' "$EXECPOL"; then printf 'exec:\n  packages:\n    git: git\n' >> "$EXECPOL"; fi
EXECOUT="$WORK/out-exec"
echo "list the git history" | "$AGENTRY" run --local testdata/runner/exec-git --policy "$EXECPOL" \
  --allow-unsigned --yes --out "$EXECOUT" --output-format json --timeout 300 >"$WORK/envelope-exec.json"
grep -q '"status": "ok"' "$WORK/envelope-exec.json" || { echo "FAIL: exec run not ok"; cat "$WORK/envelope-exec.json"; exit 1; }
grep -q 'packages resolved at image build: .*git-' "$EXECOUT/result.json" || { echo "FAIL: receipt does not name the resolved git package"; grep '"exec"' "$EXECOUT/result.json"; exit 1; }
grep -q 'safe.directory' "$EXECOUT/result.json" || { echo "FAIL: receipt does not name git's safe.directory setting"; exit 1; }
echo "exec catalog: image built, receipt names the packages"

# §R11 invariant 6: nothing of ours survives.
LEFT=$(docker ps -a --filter label=agentry.run --format '{{.Names}}')
[ -z "$LEFT" ] || { echo "FAIL: leftover containers: $LEFT"; exit 1; }
NETS=$(docker network ls --format '{{.Name}}' | grep '^agentry-' || true)
[ -z "$NETS" ] || { echo "FAIL: leftover networks: $NETS"; exit 1; }
find "$AGENTRY_RUNNER_HOME" -name secrets.env | grep -q . && { echo "FAIL: secrets.env survived"; exit 1; }
echo "teardown clean"

echo "runner-r2: all checks passed"

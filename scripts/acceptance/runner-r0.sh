#!/usr/bin/env bash
# M-R0/M-R1 acceptance (RUNNER-PLAN §R12): compile + dry-run against the
# fixture matrix, with no docker and no registry. Proves: deterministic
# plans, capability→tool mapping, the wildcard policy gate, the
# not-runnable refusal, the withdrawn-v1 toolserver refusal (exit 12),
# exit-code classes, the always-on --block-network invariant, and that
# `run --plan --json` writes no consent record.
set -euo pipefail
cd "$(dirname "$0")/../.."

AGENTRY=${AGENTRY:-bin/agentry}
if [ ! -x "$AGENTRY" ]; then
  echo "building CLI..." >&2
  make build >/dev/null
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export AGENTRY_RUNNER_HOME="$WORK/home"
# The consent records `agentry run` writes (§R1) go to the throwaway config
# dir, not the person's ~/.config/agentry/installed.yaml (agentry-cli#101).
export AGENTRY_CONFIG_DIR="$WORK/config"

# A fixed policy so plans are reproducible; Runner image pinned by name.
POLICY="$WORK/policy.yaml"
cat >"$POLICY" <<'EOF'
policyVersion: "1"
images:
  gateway: docker/mcp-gateway:v0.43.3
  harnessDockerAgent: docker/docker-agent:1.128.0
  goBuilder: golang:1.27-alpine
  runner: agentry-runner:acceptance
model:
  provider: anthropic
  name: claude-sonnet-4-5
  apiKeyEnv: ANTHROPIC_API_KEY
  proxyAllow: [api.anthropic.com:443]
allowWildcardEgress: false
secretSource:
  mode: env
budgets:
  timeoutSeconds: 900
  maxTurns: 60
# The full-caps fixture declares exec: [jq]; a declared command needs a
# catalog entry or the plan is refused (agentry-cli#100).
exec:
  packages:
    jq: jq
EOF

run_dry() { # fixture -> plan json on stdout
  "$AGENTRY" run --local "testdata/runner/$1" --dry-run --allow-unsigned --yes --policy "$POLICY" 2>/dev/null
}

fail() { echo "FAIL: $*" >&2; exit 1; }
note() { echo "ok: $*"; }

# --- pure-text: I/O channels only; networkless tools container --------------
PLAN=$(run_dry pure-text)
echo "$PLAN" | grep -q '"planVersion": "2"' || fail "pure-text: plan version"
echo "$PLAN" | grep -q '"write_report"'     || fail "pure-text: write_report grant"
echo "$PLAN" | grep -q '"list_inputs"'      || fail "pure-text: inputs channel missing (RUNNER-PLAN-2 §P4)"
echo "$PLAN" | grep -q '"fs_read"'          && fail "pure-text: fs_read must be absent"
echo "$PLAN" | grep -q -- '--block-network' || fail "pure-text: gateway must always pass --block-network"
echo "$PLAN" | grep -q '@@RUN_INPUTS@@'     || fail "pure-text: inputs placeholder missing from plan env"
note "pure-text plan"

# --- determinism: same inputs, byte-identical plans -------------------------
PLAN2=$(run_dry pure-text)
[ "$PLAN" = "$PLAN2" ] || fail "pure-text: plans differ across runs"
note "deterministic plans"

# --- full-caps: every tool granted; hosts pinned; secrets named only --------
FULL=$(FIXTURE_TOKEN=should-not-leak run_dry full-caps)
for tool in fs_read fs_list fs_write fetch run_command write_report; do
  echo "$FULL" | grep -q "\"$tool\"" || fail "full-caps: missing tool $tool"
done
echo "$FULL" | grep -q 'api.example.com'    || fail "full-caps: egress host missing"
echo "$FULL" | grep -q 'FIXTURE_TOKEN'      || fail "full-caps: secret NAME missing"
echo "$FULL" | grep -q 'should-not-leak'    && fail "full-caps: secret VALUE leaked into plan"
echo "$FULL" | grep -q '"resultKind": "json"' || fail "full-caps: output schema not wired"
note "full-caps plan"

# --- wildcard egress: refused by default policy (exit 11) -------------------
set +e
"$AGENTRY" run --local testdata/runner/wildcard --dry-run --allow-unsigned --yes --policy "$POLICY" >/dev/null 2>&1
CODE=$?
set -e
[ "$CODE" -eq 11 ] || fail "wildcard: want exit 11, got $CODE"
note "wildcard refused (exit 11)"

# --- and allowed when policy says so ----------------------------------------
sed 's/allowWildcardEgress: false/allowWildcardEgress: true/' "$POLICY" >"$WORK/policy-wild.yaml"
WILD=$("$AGENTRY" run --local testdata/runner/wildcard --dry-run --allow-unsigned --yes --policy "$WORK/policy-wild.yaml" 2>/dev/null) \
  || fail "wildcard: allowing policy still refused"
echo "$WILD" | grep -q 'UNRESTRICTED' || fail "wildcard: enforcement must disclose itself"
note "wildcard allowed by policy, loudly disclosed"

# --- withdrawn v1 mcp-server package: not runnable (exit 12) -----------------
set +e
"$AGENTRY" run --local testdata/runner/toolserver --dry-run --allow-unsigned --yes --policy "$POLICY" >/dev/null 2>&1
CODE=$?
set -e
[ "$CODE" -eq 12 ] || fail "toolserver: want exit 12, got $CODE"
note "v1 mcp-server package refused as agent (exit 12)"

# --- --local without --allow-unsigned: verify class (exit 10) ----------------
set +e
"$AGENTRY" run --local testdata/runner/pure-text --dry-run --yes --policy "$POLICY" >/dev/null 2>&1
CODE=$?
set -e
[ "$CODE" -eq 10 ] || fail "unsigned local: want exit 10, got $CODE"
note "unsigned local refused (exit 10)"

# --plan --json (agentry-cli#124): the consent-free preview compiles the
# plan, reports the attestation and the refusal a run would meet, and asks
# and records nothing — a local workspace without --allow-unsigned still
# previews, with the refusal named.
BEFORE=$(sha256sum "$WORK/config/installed.yaml" 2>/dev/null | cut -c1-16)
PLAN=$("$AGENTRY" --json run --local testdata/runner/hello-report --plan --policy "$POLICY" 2>/dev/null) \
  || fail "run --plan --json exited non-zero"
AFTER=$(sha256sum "$WORK/config/installed.yaml" 2>/dev/null | cut -c1-16)
[ "$BEFORE" = "$AFTER" ] || fail "run --plan changed the consent record"
python3 - "$PLAN" <<'PY'
import json, sys
plan = json.loads(sys.argv[1])
assert plan["attestation"] == "local" and not plan["signed"], plan["attestation"]
assert "--allow-unsigned" in plan["refusal"], plan.get("refusal")
assert plan["mode"] == "autonomous" and len(plan["planSha256"]) == 64 and plan["plan"]["ref"] == plan["ref"]
PY
note "run --plan --json previews without asking or recording"

echo "runner-r0: all checks passed"

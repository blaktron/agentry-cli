#!/usr/bin/env bash
# M-R0/M-R1 acceptance (RUNNER-PLAN §R12): compile + dry-run against the
# fixture matrix, with no docker and no registry. Proves: deterministic
# plans, capability→tool mapping, the wildcard policy gate, the
# not-runnable refusal, exit-code classes, and the always-on
# --block-network invariant.
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

# A fixed policy so plans are reproducible; Runner image pinned by name.
POLICY="$WORK/policy.yaml"
cat >"$POLICY" <<'EOF'
policyVersion: "1"
images:
  gateway: docker/mcp-gateway:v0.43.3
  harnessDockerAgent: docker/docker-agent:v1.128.0
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
EOF

run_dry() { # fixture -> plan json on stdout
  "$AGENTRY" run --local "testdata/runner/$1" --dry-run --allow-unsigned --yes --policy "$POLICY" 2>/dev/null
}

fail() { echo "FAIL: $*" >&2; exit 1; }
note() { echo "ok: $*"; }

# --- pure-text: only write_report; networkless tools container -------------
PLAN=$(run_dry pure-text)
echo "$PLAN" | grep -q '"planVersion": "1"' || fail "pure-text: plan version"
echo "$PLAN" | grep -q '"write_report"'     || fail "pure-text: write_report grant"
echo "$PLAN" | grep -q '"fs_read"'          && fail "pure-text: fs_read must be absent"
echo "$PLAN" | grep -q -- '--block-network' || fail "pure-text: gateway must always pass --block-network"
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

# --- mcp-server package: not runnable (exit 12) ------------------------------
set +e
"$AGENTRY" run --local testdata/runner/toolserver --dry-run --allow-unsigned --yes --policy "$POLICY" >/dev/null 2>&1
CODE=$?
set -e
[ "$CODE" -eq 12 ] || fail "toolserver: want exit 12, got $CODE"
note "mcp-server refused as agent (exit 12)"

# --- --local without --allow-unsigned: verify class (exit 10) ----------------
set +e
"$AGENTRY" run --local testdata/runner/pure-text --dry-run --yes --policy "$POLICY" >/dev/null 2>&1
CODE=$?
set -e
[ "$CODE" -eq 10 ] || fail "unsigned local: want exit 10, got $CODE"
note "unsigned local refused (exit 10)"

echo "runner-r0: all checks passed"

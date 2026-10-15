#!/usr/bin/env bash
# Runner acceptance, compile and dry run against the fixture matrix, with no
# docker and no registry. Proves: deterministic plans, capability→tool
# mapping, the wildcard policy gate, the not-runnable refusal, the
# withdrawn-v1 toolserver refusal (exit 12), exit-code classes, the
# always-on --block-network invariant, and that `run --plan --json` writes
# no consent record.
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
# The consent records `agentry run` writes go to the throwaway config dir,
# not the person's ~/.config/agentry/installed.yaml.
export AGENTRY_CONFIG_DIR="$WORK/config"

# A fixed policy so plans are reproducible; Runner image pinned by name.
POLICY="$WORK/policy.yaml"
cat >"$POLICY" <<'EOF'
policyVersion: "1"
images:
  gateway: docker/mcp-gateway:v0.43.3
  harnessDockerAgent: docker/docker-agent:1.150.0
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
# catalog entry or the plan is refused.
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
grep -q '"planVersion": "2"' <<<"$PLAN" || fail "pure-text: plan version"
grep -q '"write_report"' <<<"$PLAN"     || fail "pure-text: write_report grant"
grep -q '"list_inputs"' <<<"$PLAN"      || fail "pure-text: inputs channel missing"
grep -q '"fs_read"' <<<"$PLAN"          && fail "pure-text: fs_read must be absent"
grep -q -- '--block-network' <<<"$PLAN" || fail "pure-text: gateway must always pass --block-network"
grep -q '@@RUN_INPUTS@@' <<<"$PLAN"     || fail "pure-text: inputs placeholder missing from plan env"
note "pure-text plan"

# --- determinism: same inputs, byte-identical plans -------------------------
PLAN2=$(run_dry pure-text)
[ "$PLAN" = "$PLAN2" ] || fail "pure-text: plans differ across runs"
note "deterministic plans"

# --- full-caps: every tool granted; hosts pinned; secrets named only --------
FULL=$(FIXTURE_TOKEN=should-not-leak run_dry full-caps)
for tool in fs_read fs_list fs_write fetch run_command write_report; do
  grep -q "\"$tool\"" <<<"$FULL" || fail "full-caps: missing tool $tool"
done
grep -q 'api.example.com' <<<"$FULL"    || fail "full-caps: egress host missing"
grep -q 'FIXTURE_TOKEN' <<<"$FULL"      || fail "full-caps: secret NAME missing"
grep -q 'should-not-leak' <<<"$FULL"    && fail "full-caps: secret VALUE leaked into plan"
grep -q '"resultKind": "json"' <<<"$FULL" || fail "full-caps: output schema not wired"
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
grep -q 'UNRESTRICTED' <<<"$WILD" || fail "wildcard: enforcement must disclose itself"
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

# --plan --json: the consent-free preview compiles the
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

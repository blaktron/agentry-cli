#!/usr/bin/env bash
# Runner acceptance, the real execute path on a plain docker engine:
# first-use image install, gateway + tools + proxy + agent containers,
# result envelope, receipts, and the no-leak teardown sweep.
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
  echo "SKIP: no reachable docker daemon" >&2
  exit 0
fi
# The model comes from the operator policy when one exists: its apiKeyEnv
# names the variable that must be set. Without a policy the runner's built-in
# default (anthropic, ANTHROPIC_API_KEY) applies.
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
# The leak sweep runs whatever happened: under set -e a failed run would
# skip it, and "teardown on every path" would be checked on the success path
# only. A leak found after a failure is reported beside the failure; the exit
# status stays the failure's.
RUN_PID=""
sweep_leftovers() {
  local rc=$?
  local left nets vols
  # A run still in flight (an assertion failed while it ran) gets to finish
  # and tear down before its resources are judged.
  if [ -n "$RUN_PID" ] && kill -0 "$RUN_PID" 2>/dev/null; then wait "$RUN_PID" || true; fi
  left=$(docker ps -a --filter label=agentry.run --format '{{.Names}}' || true)
  nets=$(docker network ls --format '{{.Name}}' | grep '^agentry-' || true)
  vols=$(docker volume ls --filter label=agentry.run --format '{{.Name}}' || true)
  if [ -n "$left$nets$vols" ]; then
    echo "FAIL: run resources survived teardown — containers: [$left] networks: [$nets] volumes: [$vols]" >&2
    [ $rc -eq 0 ] && rc=1
  fi
  rm -rf "$WORK"
  exit $rc
}
trap sweep_leftovers EXIT
# The gateway's daemon socket is a per-run filter: the
# receipt names it, and a run whose gateway was refused anything fails
# here with the refusals, since a working run's gateway needs nothing the
# filter denies.
filter_refusals() { grep ' refuse ' "$1/receipts/docker-filter.log" 2>/dev/null || true; }
check_filter() {
  local out=$1 refused
  [ -f "$out/receipts/docker-filter.log" ] || { echo "FAIL: no docker filter log in $out/receipts"; exit 1; }
  grep -q "only through this run's filter" "$out/result.json" || { echo "FAIL: the receipt does not name the gateway's docker filter"; exit 1; }
  refused=$(filter_refusals "$out")
  [ -z "$refused" ] || { echo "FAIL: the docker filter refused the gateway:"; echo "$refused"; exit 1; }
}
export AGENTRY_RUNNER_HOME="$WORK/home"
# The consent records `agentry run` writes go to the throwaway config dir,
# not the person's ~/.config/agentry/installed.yaml.
export AGENTRY_CONFIG_DIR="$WORK/config"
OUT="$WORK/out"

# --prepare: verify + compile + pull/build images, no execution.
"$AGENTRY" run --local testdata/runner/hello-report --allow-unsigned --yes ${POLICY_FLAG[@]+"${POLICY_FLAG[@]}"} --prepare
echo "prepare ok"

# The run itself: task on stdin, envelope on stdout. It runs in the
# background so the containers can be inspected while they are live —
# the gateway never runs without --block-network, and the agent container is
# internal-only with no published ports and no secret in its environment.
echo "please make the report" | "$AGENTRY" run --local testdata/runner/hello-report ${POLICY_FLAG[@]+"${POLICY_FLAG[@]}"} \
  --allow-unsigned --yes --out "$OUT" --output-format json --timeout 300 >"$WORK/envelope.json" &
RUN_PID=$!
AGENT=""
for _ in $(seq 1 120); do
  AGENT=$(docker ps --filter label=agentry.run --format '{{.Names}}' | grep -- '-agent$' | head -1 || true)
  [ -n "$AGENT" ] && break
  kill -0 "$RUN_PID" 2>/dev/null || break
  sleep 1
done
if [ -n "$AGENT" ]; then
  RUN_ID=${AGENT#agentry-}; RUN_ID=${RUN_ID%-agent}
  GW="agentry-$RUN_ID-gateway"
  grep -q -- '--block-network' <<<"$(docker inspect "$GW" --format '{{join .Args " "}}')" || { echo "FAIL: live gateway $GW runs without --block-network"; exit 1; }
  NETS=$(docker inspect "$AGENT" --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}')
  [ "$(echo $NETS | wc -w)" -eq 1 ] || { echo "FAIL: agent container on more than one network: $NETS"; exit 1; }
  grep -qx true <<<"$(docker network inspect $NETS --format '{{.Internal}}')" || { echo "FAIL: agent network $NETS is not internal"; exit 1; }
  PORTS=$(docker inspect "$AGENT" --format '{{json .NetworkSettings.Ports}}')
  grep -q 'HostPort' <<<"$PORTS" && { echo "FAIL: agent container publishes a port: $PORTS"; exit 1; }
  # The agent holds the model key under the provider's name by necessity
  # and the gateway token it calls the gateway with; what it must never hold
  # is a registry credential (nothing in pkg/runner reads AGENTRY_TOKEN or
  # SECUREAGENTRY_TOKEN) or the daemon's socket (the gateway alone has one,
  # through the filter).
  grep -qE '^(SECURE)?AGENTRY_TOKEN=' <<<"$(docker inspect "$AGENT" --format '{{join .Config.Env "\n"}}')" && { echo "FAIL: a registry token is in the agent container's environment"; exit 1; }
  grep -q 'docker.sock' <<<"$(docker inspect "$AGENT" --format '{{range .Mounts}}{{.Source}} {{end}}')" && { echo "FAIL: the agent container mounts the docker socket"; exit 1; }
  # The container posture: every capability dropped and
  # no-new-privileges on the agent and the gateway, a read-only root and a
  # pids and memory bound on the agent, and the bounds passed to what the
  # gateway launches.
  for C in "$AGENT" "$GW"; do
    grep -q '"ALL"' <<<"$(docker inspect "$C" --format '{{json .HostConfig.CapDrop}}')" || { echo "FAIL: $C keeps its capabilities"; exit 1; }
    grep -q 'no-new-privileges' <<<"$(docker inspect "$C" --format '{{json .HostConfig.SecurityOpt}}')" || { echo "FAIL: $C runs without no-new-privileges"; exit 1; }
  done
  grep -qx true <<<"$(docker inspect "$AGENT" --format '{{.HostConfig.ReadonlyRootfs}}')" || { echo "FAIL: the agent container's root is writable"; exit 1; }
  grep -Eq '^[1-9][0-9]* [1-9][0-9]*$' <<<"$(docker inspect "$AGENT" --format '{{.HostConfig.PidsLimit}} {{.HostConfig.Memory}}')" || { echo "FAIL: the agent container has no pids or memory bound"; exit 1; }
  grep -Eq -- '--cpus [0-9]+ --memory [0-9]+[mg]' <<<"$(docker inspect "$GW" --format '{{join .Args " "}}')" || { echo "FAIL: the gateway does not bound what it launches"; exit 1; }
  grep -q '/var/run/docker.sock' <<<"$(docker inspect "$GW" --format '{{range .Mounts}}{{.Source}} {{end}}')" && { echo "FAIL: the gateway mounts the daemon's own socket, not the run's filter"; exit 1; }
  echo "live containers: gateway --block-network, agent internal-only on $NETS, no published port, no registry token, no socket"
  echo "live posture: agent and gateway drop ALL with no-new-privileges; agent root read-only with pids and memory bounds; gateway passes cpus and memory on; gateway's socket is the run's filter"
else
  echo "WARN: the agent container was never observed live (the run ended first?)" >&2
fi
wait "$RUN_PID" || { echo "FAIL: run exited non-zero"; cat "$WORK/envelope.json"; filter_refusals "$OUT"; exit 1; }

grep -q '"status": "ok"' "$WORK/envelope.json" || { echo "FAIL: run not ok"; cat "$WORK/envelope.json"; exit 1; }
grep -q '"runPlanSha256"' "$WORK/envelope.json" || { echo "FAIL: envelope lacks plan hash"; exit 1; }
[ -f "$OUT/result.json" ] || { echo "FAIL: result.json missing"; exit 1; }
[ -f "$OUT/receipts/events.ndjson" ] || { echo "FAIL: events receipt missing"; exit 1; }
[ -f "$OUT/receipts/run-plan.json" ] || { echo "FAIL: plan receipt missing"; exit 1; }
if [ ! -s "$OUT/reports/hello.md" ]; then
  echo "WARN: agent did not write reports/hello.md (model behavior — inspect receipts)" >&2
fi
check_filter "$OUT"
echo "envelope + receipts ok; the docker filter refused nothing"

# docker.runtime: when the operator policy names a runtime, the receipt names
# it too, class by class. The receipt line is the runner's claim, so it is
# asserted only when the policy made one.
if [ -f "$POLICY" ] && RUNTIME=$(awk '/^docker:/{d=1; next} d && /^[^ ]/{d=0} d && /^ *runtime:/{print $2; exit}' "$POLICY") && [ -n "$RUNTIME" ]; then
  grep -q "\"runtime\": \"$RUNTIME:" "$OUT/result.json" || { echo "FAIL: policy names docker.runtime $RUNTIME but the receipt does not"; grep '"runtime"' "$OUT/result.json"; exit 1; }
  grep -q "the gateway's health probe" <<<"$(grep '"runtime":' "$OUT/result.json")" || { echo "FAIL: the receipt's runtime line does not name each class"; grep '"runtime"' "$OUT/result.json"; exit 1; }
  echo "docker.runtime $RUNTIME: receipt names it, class by class"
else
  if grep -q '"runtime":' "$OUT/result.json"; then echo "FAIL: the receipt names a runtime the policy never set"; exit 1; fi
fi

# The exec catalog: a QuickAgent declaring exec [git]
# runs in an exec image built from the catalogued package, and the receipt
# names the packages the build resolved. The model's use of git is not
# asserted (its tool-call shape varies); the build, the record and the
# receipt line are.
EXECPOL="$WORK/policy-exec.yaml"
# With AGENTRY_RUNNER_HOME exported above, --prepare writes its default
# policy under that home, not the user's.
if [ -f "$POLICY" ]; then cp "$POLICY" "$EXECPOL"; else "$AGENTRY" run --local testdata/runner/hello-report --allow-unsigned --yes --prepare >/dev/null 2>&1; cp "$AGENTRY_RUNNER_HOME/policy.yaml" "$EXECPOL"; fi
if ! grep -q '^exec:' "$EXECPOL"; then printf 'exec:\n  packages:\n    git: git\n' >> "$EXECPOL"; fi
EXECOUT="$WORK/out-exec"
echo "list the git history" | "$AGENTRY" run --local testdata/runner/exec-git --policy "$EXECPOL" \
  --allow-unsigned --yes --out "$EXECOUT" --output-format json --timeout 300 >"$WORK/envelope-exec.json"
grep -q '"status": "ok"' "$WORK/envelope-exec.json" || { echo "FAIL: exec run not ok"; cat "$WORK/envelope-exec.json"; exit 1; }
grep -q 'packages resolved at image build: .*git-' "$EXECOUT/result.json" || { echo "FAIL: receipt does not name the resolved git package"; grep '"exec"' "$EXECOUT/result.json"; exit 1; }
grep -q 'safe.directory' "$EXECOUT/result.json" || { echo "FAIL: receipt does not name git's safe.directory setting"; exit 1; }
check_filter "$EXECOUT"
echo "exec catalog: image built, receipt names the packages"

# A declared MCP server: the gateway starts relay sidecars for it, and the
# teardown assertion below is about them. The
# fixture pins the same server the examples use; the install needs the
# registry (npm) reachable from the docker host.
MCPOUT="$WORK/out-mcp"
echo "look up GHSA-8v63-cqqc-6r2c in OSV and summarise it" | "$AGENTRY" run --local testdata/runner/mcp-osv ${POLICY_FLAG[@]+"${POLICY_FLAG[@]}"} \
  --allow-unsigned --yes --out "$MCPOUT" --output-format json --timeout 300 >"$WORK/envelope-mcp.json" \
  || { echo "FAIL: declared-server run exited non-zero"; cat "$WORK/envelope-mcp.json"; filter_refusals "$MCPOUT"; exit 1; }
grep -q '"status": "ok"' "$WORK/envelope-mcp.json" || { echo "FAIL: declared-server run not ok"; cat "$WORK/envelope-mcp.json"; exit 1; }
check_filter "$MCPOUT"
echo "declared MCP server: run ok, relays launched through the docker filter"

# A declared remote MCP server: the gateway launches the
# runner image's bridge for it like any declared server, behind one relay to
# the server's host, through the docker filter. The fixture's server is
# DeepWiki's public one (no sign-in), so the docker host needs to reach
# mcp.deepwiki.com:443.
REMOTEOUT="$WORK/out-remote"
echo "modelcontextprotocol/go-sdk" | "$AGENTRY" run --local testdata/runner/mcp-remote ${POLICY_FLAG[@]+"${POLICY_FLAG[@]}"} \
  --allow-unsigned --yes --out "$REMOTEOUT" --output-format json --timeout 300 >"$WORK/envelope-remote.json" \
  || { echo "FAIL: remote-server run exited non-zero"; cat "$WORK/envelope-remote.json"; filter_refusals "$REMOTEOUT"; exit 1; }
grep -q '"status": "ok"' "$WORK/envelope-remote.json" || { echo "FAIL: remote-server run not ok"; cat "$WORK/envelope-remote.json"; exit 1; }
check_filter "$REMOTEOUT"
grep -q 'https://mcp.deepwiki.com/mcp (auth none' "$REMOTEOUT/result.json" || { echo "FAIL: the receipt does not name the remote server and its auth"; exit 1; }
grep -qE 'docker-mcp-l[47]proxy-[^ ]+ for mcp\.deepwiki\.com' "$REMOTEOUT/receipts/gateway.log" || { echo "FAIL: no relay to mcp.deepwiki.com named in the remote run's gateway.log"; exit 1; }
echo "declared remote MCP server: run ok, bridged behind one relay through the docker filter"

# Nothing of ours survives. (The EXIT trap repeats this
# on every path; here it is the success-path assertion with the names.)
LEFT=$(docker ps -a --filter label=agentry.run --format '{{.Names}}')
[ -z "$LEFT" ] || { echo "FAIL: leftover containers: $LEFT"; exit 1; }
NETS=$(docker network ls --format '{{.Name}}' | grep '^agentry-' || true)
[ -z "$NETS" ] || { echo "FAIL: leftover networks: $NETS"; exit 1; }
VOLS=$(docker volume ls --filter label=agentry.run --format '{{.Name}}')
[ -z "$VOLS" ] || { echo "FAIL: leftover volumes: $VOLS"; exit 1; }
grep -q . <<<"$(find "$AGENTRY_RUNNER_HOME" -name secrets.env)" && { echo "FAIL: secrets.env survived"; exit 1; }
grep -q . <<<"$(find "$AGENTRY_RUNNER_HOME" -type d -name connections -path '*/cfg/*')" && { echo "FAIL: a run's connection folders survived"; exit 1; }
# The relay sidecars the gateway started for a declared server are reaped
# by the names its log gave them: none named in any
# run's gateway.log receipt may survive. Checked by name, never by label —
# the user's own docker mcp gateway may have relays of its own. The list
# must be non-empty: the mcp-osv run above declares a server, so an empty
# list means the assertion is looping over nothing.
RELAYS=$(cat "$OUT"/receipts/gateway.log "$EXECOUT"/receipts/gateway.log "$MCPOUT"/receipts/gateway.log "$REMOTEOUT"/receipts/gateway.log 2>/dev/null | grep -oE '^(starting l4 proxy docker-mcp-l4proxy-[^ ]+ for|[[:space:]]+- Starting l7 proxy docker-mcp-l7proxy-[^ ]+ for)' | grep -oE 'docker-mcp-l[47]proxy-[^ ]+' | sort -u || true)
[ -n "$RELAYS" ] || { echo "FAIL: no relay sidecar named in any gateway.log — the declared-server fixture started none, so the assertion proves nothing"; exit 1; }
for r in $RELAYS; do
  grep -qx "$r" <<<"$(docker ps -a --format '{{.Names}}')" && { echo "FAIL: relay sidecar $r survived teardown"; exit 1; }
done
echo "teardown clean ($(echo $RELAYS | wc -w) relay sidecar(s) named and reaped)"

echo "runner-r2: all checks passed"

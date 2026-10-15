#!/usr/bin/env bash
# Shared-host acceptance: two runs whose declared MCP servers need relays
# run at once on one docker host, as a sandbox host with capacity above one
# runs them. Both must succeed, each run's relays must sit on that run's own
# two networks, and neither run's relay network may resolve or reach the
# other run's relay. Linux only: elsewhere the gateway keeps the host's one relay network.
#
# Requirements, as runner-r2.sh: a reachable docker daemon, the model key
# the operator policy names in the environment, network access to pull.
set -uo pipefail
cd "$(dirname "$0")/../.."
[ "$(uname -s)" = Linux ] || { echo "SKIP: the per-run relay networks are Linux's"; exit 0; }
docker info >/dev/null 2>&1 || { echo "SKIP: no docker daemon"; exit 0; }
POLICY=${AGENTRY_POLICY:-$HOME/.agentry/runner/policy.yaml}
POLICY_FLAG=()
KEY_ENV=ANTHROPIC_API_KEY
if [ -f "$POLICY" ]; then
  POLICY_FLAG=(--policy "$POLICY")
  KEY_ENV=$(awk '/apiKeyEnv:/{print $2; exit}' "$POLICY")
fi
[ -n "${!KEY_ENV:-}" ] || { echo "SKIP: $KEY_ENV not set"; exit 0; }
AGENTRY=${AGENTRY:-bin/agentry}
[ -x "$AGENTRY" ] || { echo "FAIL: $AGENTRY not built (make build)"; exit 1; }

W=$(mktemp -d)
A="" B=""
trap '[ -n "$A" ] && kill $A 2>/dev/null; [ -n "$B" ] && kill $B 2>/dev/null; wait 2>/dev/null; rm -rf "$W"' EXIT
# The host's own relay networks (a user's `docker mcp gateway`) must come
# through untouched: their ids now and at the end.
fixed() { docker network ls --filter name=docker-mcp-proxies --format '{{.ID}} {{.Name}}' | sort; }
FIXED_BEFORE=$(fixed)
# One runner home, as runhost has: the runs share it.
export AGENTRY_RUNNER_HOME="$W/home" AGENTRY_CONFIG_HOME="$W/cfg"
fail() { echo "FAIL: $*"; exit 1; }
run() {
  echo "look up GHSA-8v63-cqqc-6r2c in OSV and summarise it" | "$AGENTRY" run --local testdata/runner/mcp-osv ${POLICY_FLAG[@]+"${POLICY_FLAG[@]}"} \
    --allow-unsigned --yes --out "$W/out-$1" --output-format json --timeout 300 >"$W/env-$1.json" 2>"$W/err-$1.log"
}
# Install once, so the two runs overlap on their gateways and relays rather
# than on the server's npm install.
echo "hi" | "$AGENTRY" run --local testdata/runner/mcp-osv ${POLICY_FLAG[@]+"${POLICY_FLAG[@]}"} --allow-unsigned --yes --prepare >/dev/null 2>&1 \
  || fail "prepare"
run a & A=$!
sleep 2
run b & B=$!

# One relay of each of two runs, live together.
pick() { docker ps --filter label=docker-mcp-proxy=true --filter label=agentry.gateway.run --format '{{.Label "agentry.run"}} {{.Names}}' | sort -u -k1,1; }
pairs=""
for _ in $(seq 1 150); do
  pairs=$(pick)
  [ "$(printf '%s\n' "$pairs" | grep -c .)" -ge 2 ] && break
  sleep 1
done
[ "$(printf '%s\n' "$pairs" | grep -c .)" -ge 2 ] || fail "the two runs' relays were never live together"
runa=$(printf '%s\n' "$pairs" | sed -n 1p | cut -d' ' -f1); ra=$(printf '%s\n' "$pairs" | sed -n 1p | cut -d' ' -f2)
runb=$(printf '%s\n' "$pairs" | sed -n 2p | cut -d' ' -f1); rb=$(printf '%s\n' "$pairs" | sed -n 2p | cut -d' ' -f2)
for pair in "$ra:$runa" "$rb:$runb"; do
  r=${pair%%:*} id=${pair#*:}
  nets=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$r")
  [ "$(echo $nets)" = "agentry-$id-relays agentry-$id-relays-out" ] || fail "relay $r of run $id is on: $nets"
done
echo "overlap: relays of runs $runa and $runb live together, each on its run's own two networks"
# From each run's relay network: its own relay must answer (the probe
# works), the other run's must not, by name or by address.
probe() { # network relay -> "RESOLVED REACHED" as found, by name and on any of its addresses
  local ips
  ips=$(docker inspect -f "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}" "$2")
  docker run --rm --network "$1" alpine:3 sh -c \
    "if ping -c 1 -W 1 $2 2>&1 | grep -q '^PING'; then echo RESOLVED; fi; for ip in $ips; do if nc -z -w 3 \$ip 443 2>/dev/null; then echo REACHED; break; fi; done" 2>/dev/null | tr '\n' ' '
}
for dir in "$runa $ra $runb $rb" "$runb $rb $runa $ra"; do
  set -- $dir
  own=$(probe "agentry-$1-relays" "$2")
  case "$own" in *RESOLVED*REACHED*) ;; *) fail "control: run $1's relay network does not reach its own relay $2 ($own) — the probe proves nothing" ;; esac
  other=$(probe "agentry-$1-relays" "$4")
  case "$other" in *RESOLVED*|*REACHED*) fail "run $1's relay network reaches run $3's relay $4: $other" ;; esac
done
echo "isolation: each run's relay network reaches its own relay and neither resolves nor reaches the other's"

wait $A || fail "run a exited non-zero: $(cat "$W/env-a.json")"
A=""
wait $B || fail "run b exited non-zero: $(cat "$W/env-b.json")"
B=""
for x in a b; do
  grep -q '"status": "ok"' "$W/env-$x.json" || fail "run $x not ok"
  [ -f "$W/out-$x/receipts/docker-filter.log" ] || fail "run $x has no docker filter log"
  if grep -q ' refuse ' "$W/out-$x/receipts/docker-filter.log"; then fail "the docker filter refused run $x's gateway: $(grep ' refuse ' "$W/out-$x/receipts/docker-filter.log")"; fi
done
left=$(docker network ls --format '{{.Name}}' | grep -E "^agentry-($runa|$runb)-" || true)
[ -z "$left" ] || fail "relay networks survived their runs: $left"
[ "$(fixed)" = "$FIXED_BEFORE" ] || fail "the host's own relay networks changed: before [$FIXED_BEFORE] after [$(fixed)]"
echo "both runs ok, the filter refused nothing, their relay networks removed, the host's own untouched"

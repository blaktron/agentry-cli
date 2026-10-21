#!/usr/bin/env bash
# Container engine conformance: every command the runner's engine client
# (pkg/runner/dockerx) issues, spelled as it spells them, against the engine
# this environment names; then a measurement of what a sandboxed run still
# needs from that engine; then one built-in run on it.
#
#   scripts/acceptance/engine.sh
#
# The engine:
#   AGENTRY_ENGINE=podman  podman's own CLI (AGENTRY_PODMAN names the binary).
#                          Its API socket must answer, for the run's docker
#                          filter and the gateway:
#                          `systemctl --user enable --now podman.socket`.
#   otherwise              the docker CLI against its current context:
#                          DOCKER_HOST and DOCKER_CONTEXT as it honours them,
#                          which may be podman's socket.
# AGENTRY (default bin/agentry) is the CLI under test.
# scripts/acceptance/engine-podman.sh runs this script against a rootless
# podman of its own, in a container on a machine that has only Docker Engine.
#
# The built-in run (the Knowledge Creator over one document, no web) needs the
# model the policy names (AGENTRY_POLICY, default ~/.agentry/runner/policy.yaml)
# and its key in the environment; without them it is skipped and said so. It
# runs in a throwaway runner home and config directory.
#
# RUNSC names a gVisor runsc binary for the measurements a sandboxed run
# needs; without it they are reported as not measured. With it, on podman,
# the script registers runsc and a --host-uds=open wrapper for the test
# alone (CONTAINERS_CONF_OVERRIDE) and runs a QuickAgent with declared
# egress and exec under them; on docker it does the same when the daemon
# registers runsc and runsc-uds. That run's model is a scripted stand-in
# served from this host (python3), so it needs no key: it asks for the fetch
# tool once, writes a report, and answers with the page's title. Every
# container of the run is inspected while it is up, and each must be under
# the runtime; the report must come out owned by the person.
#
# ENGINE_KEEP=1 keeps the work directory (the sandboxed run's receipts and
# logs among them) for a look after a failure; it is removed otherwise.
#
# Each line is PASS, FAIL, GAP (where this engine differs from Docker Engine:
# the line says whether the client handles it), MEASURED (a fact reported, not
# judged) or SKIP. The exit status is 1 when any line is FAIL.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

AGENTRY=${AGENTRY:-bin/agentry}
[ -x "$AGENTRY" ] || make build >/dev/null
AGENTRY=$(cd "$(dirname "$AGENTRY")" && pwd)/$(basename "$AGENTRY")
# The probe image is the project's own copy of alpine 3.22 in GHCR, pinned by
# digest like the runtime images: an anonymous pull from Docker Hub is rate
# limited on a shared address, and then every step fails before it tests the
# engine.
IMG=${ENGINE_PROBE_IMAGE:-ghcr.io/blaktron/agentry-alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8}

if [ "${AGENTRY_ENGINE:-}" = podman ]; then
  C=${AGENTRY_PODMAN:-podman}
  KIND_CLI=podman
else
  C=${AGENTRY_DOCKER:-docker}
  KIND_CLI=docker
fi
command -v "$C" >/dev/null || { echo "SKIP: no $C on PATH" >&2; exit 0; }

FAILS=0
TAG="engine-check-$$"
WORK=$(mktemp -d "${HOME}/.agentry-engine-check.XXXXXX")
# The person's own directory, for the bind checks: the system temp directory
# may be a tmpfs a VM engine does not share.
chmod 700 "$WORK"
# The docker CLI's build plugin may start a builder container of its own on
# an engine whose API serves no BuildKit; one this script started goes too.
BUILDERS_BEFORE=$("$C" ps -a --format '{{.Names}}' 2>/dev/null | grep '^buildx_buildkit_' | sort)
cleanup() {
  local ids b
  ids=$("$C" ps -aq --filter "label=agentry.engine-check=$TAG" 2>/dev/null)
  [ -n "$ids" ] && "$C" rm -fv $ids >/dev/null 2>&1
  for n in $("$C" network ls -q --filter "label=agentry.engine-check=$TAG" 2>/dev/null); do "$C" network rm "$n" >/dev/null 2>&1; done
  for v in $("$C" volume ls -q --filter "label=agentry.engine-check=$TAG" 2>/dev/null); do "$C" volume rm -f "$v" >/dev/null 2>&1; done
  "$C" image rm -f "agentry-engine-check:$TAG" >/dev/null 2>&1
  for b in $(comm -13 <(echo "$BUILDERS_BEFORE") <("$C" ps -a --format '{{.Names}}' 2>/dev/null | grep '^buildx_buildkit_' | sort)); do
    "$C" rm -fv "$b" >/dev/null 2>&1; "$C" volume rm -f "${b}_state" >/dev/null 2>&1
  done
  # ENGINE_KEEP keeps the work directory (the sandboxed run's receipts and
  # logs among them) for a look after a failure.
  [ -n "${ENGINE_KEEP:-}" ] && echo "kept: $WORK" || rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

line() { printf '%-9s %-34s %s\n' "$1" "$2" "$3"; [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
L="--label agentry.engine-check=$TAG"

# --- What the engine says it is ----------------------------------------------
if [ "$KIND_CLI" = podman ]; then
  ver=$("$C" version --format '{{.Client.Version}}' 2>&1) && line PASS "version (client: podman runs itself)" "$ver" || line FAIL "version" "$ver"
  arch=$("$C" version --format '{{.Client.OsArch}}' 2>&1) && line PASS "version .Client.OsArch" "$arch" || line FAIL "version .Client.OsArch" "$arch"
  info=$("$C" info --format json 2>&1) || { line FAIL "info" "$info"; exit 1; }
  read -r ROOTLESS SOCK RT DEFNET HOSTN KERN < <(printf '%s' "$info" | python3 -c '
import json,sys; h=json.load(sys.stdin)["host"]
print(str(h["security"]["rootless"]).lower(), h["remoteSocket"]["path"], h["ociRuntime"]["name"], h.get("networkBackendInfo",{}).get("defaultNetwork","podman"), h["hostname"], h["kernel"])')
  line PASS "info" "rootless=$ROOTLESS socket=$SOCK runtime=$RT default network=$DEFNET daemon=$HOSTN/$KERN"
  if curl -sf --unix-socket "$SOCK" http://d/_ping >/dev/null 2>&1; then
    line PASS "API socket answers /_ping" "$SOCK"
  else
    line FAIL "API socket answers /_ping" "$SOCK does not answer: systemctl --user enable --now podman.socket"
  fi
  "$C" version --format '{{.Server.Version}}' >/dev/null 2>&1 && line MEASURED "version .Server" "answered" || line GAP "version .Server" "podman has no .Server locally; the client reads .Client (handled)"
else
  ep=$("$C" context inspect --format '{{.Endpoints.docker.Host}}' 2>&1) && line PASS "context endpoint" "$ep" || line FAIL "context endpoint" "$ep"
  sv=$("$C" version --format '{{.Server.Version}} {{.Server.Arch}} {{(index .Server.Components 0).Name}}' 2>&1) && line PASS "version .Server" "$sv" || { line FAIL "version .Server" "$sv"; exit 1; }
  info=$("$C" info --format '{{.Name}} {{.KernelVersion}} {{json .SecurityOptions}}' 2>&1) && line PASS "info name, kernel, security" "$info" || line FAIL "info" "$info"
  rts=$("$C" info --format '{{json .Runtimes}}' 2>&1) && line PASS "info .Runtimes" "$(printf '%s' "$rts" | python3 -c 'import json,sys; print(" ".join(sorted(json.load(sys.stdin))))')" || line FAIL "info .Runtimes" "$rts"
  ROOTLESS=false; case "$info" in *name=rootless*) ROOTLESS=true ;; esac
  SOCK=${ep#unix://}
  DEFNET=bridge
  case "$sv" in *"Podman Engine"*) line GAP "kind" "podman's API through the docker CLI: a working route, not the supported one (AGENTRY_ENGINE=podman)" ;; esac
fi
echo "engine line: $("$AGENTRY" doctor 2>&1 | awk '$2 == "docker" {$1=""; $2=""; sub(/^ +/, ""); print; exit}')"

# --- Networks and volumes ----------------------------------------------------
NET="agentry-ec-$$-net"
"$C" pull -q "$IMG" >/dev/null 2>&1 || "$C" pull "$IMG" >/dev/null
out=$("$C" network create --driver bridge --internal $L "$NET" 2>&1) && line PASS "network create --internal --label" "$NET" || line FAIL "network create" "$out"
[ "$("$C" network ls --filter "label=agentry.engine-check=$TAG" --format '{{.Name}}')" = "$NET" ] && line PASS "network ls --filter label" "" || line FAIL "network ls --filter label" "not listed"
VOL="agentry-ec-$$-vol"
"$C" volume create $L "$VOL" >/dev/null && [ "$("$C" volume ls --filter "label=agentry.engine-check=$TAG" --format '{{.Name}}')" = "$VOL" ] \
  && line PASS "volume create --label, ls --filter" "" || line FAIL "volume create --label" "not listed"
out=$("$C" create --name "ec-$$-anon" $L --mount "type=volume,dst=/v,volume-label=agentry.engine-check=$TAG,volume-label=ec.anon=1" "$IMG" true 2>&1)
if [ $? -ne 0 ]; then
  line GAP "--mount volume-label (anonymous)" "refused (${out##*Error: }); the client makes a named labelled volume instead (handled)"
elif [ -n "$("$C" volume ls -q --filter label=ec.anon=1)" ]; then
  line PASS "--mount volume-label (anonymous)" "labelled"
else
  line GAP "--mount volume-label (anonymous)" "accepted but the label dropped, so a leaked one escapes the label sweep (not handled on this route)"
fi
"$C" rm -fv "ec-$$-anon" >/dev/null 2>&1

# --- A hardened container, as the runner starts one --------------------------
mkdir -p "$WORK/bind" && echo bound > "$WORK/bind/f"
out=$("$C" run -d --name "ec-$$-a" $L --hostname ec-a --network "$NET" --network-alias ec-alias \
  --add-host host.docker.internal:host-gateway --security-opt label=disable \
  --cap-drop ALL --security-opt no-new-privileges --read-only --tmpfs /tmp:size=16m \
  --pids-limit 100 --memory 256m --cpus 1 --mount "type=volume,src=$VOL,dst=/vol" \
  -v "$WORK/bind:/bind:ro,z" "$IMG" sleep 300 2>&1) && line PASS "run -d with the runner's posture" "" || line FAIL "run -d with the runner's posture" "$out"
inside=$("$C" exec "ec-$$-a" sh -c '
  grep CapEff /proc/self/status | tr -s "\t" " "; grep NoNewPrivs /proc/self/status | tr -s "\t" " "
  touch /x 2>/dev/null && echo root-writable || echo root-read-only
  touch /tmp/x && echo tmpfs-writable; cat /bind/f; touch /vol/x && echo volume-writable
  cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/pids.max /sys/fs/cgroup/cpu.max 2>/dev/null | tr "\n" " "; echo
  getent hosts host.docker.internal | cut -d" " -f1' 2>&1)
case "$inside" in
  *"CapEff: 0000000000000000"*"NoNewPrivs: 1"*root-read-only*tmpfs-writable*bound*volume-writable*) line PASS "posture inside" "$(echo "$inside" | sed -n 6,7p | tr '\n' ' ')" ;;
  *) line FAIL "posture inside" "$(echo "$inside" | tr '\n' ' ')" ;;
esac
alias=$("$C" run --rm $L --network "$NET" "$IMG" sh -c 'getent hosts ec-alias && getent hosts ec-a' 2>&1) && line PASS "name and alias DNS (--internal)" "$(sed -n 1p <<<"$alias")" || line FAIL "name and alias DNS (--internal)" "$alias"
"$C" run -d --name "ec-$$-p" $L -p 127.0.0.1::8080 "$IMG" sleep 300 >/dev/null
port=$("$C" port "ec-$$-p" 8080/tcp 2>&1) && line PASS "port" "$port" || line FAIL "port" "$port"
lbl=$("$C" ps -a --filter "label=agentry.engine-check=$TAG" --format '{{.Names}}	{{.Label "agentry.engine-check"}}' | sort | sed -n 1p) && [ -n "$lbl" ] && line PASS "ps --filter label, {{.Label}}" "$lbl" || line FAIL "ps {{.Label}}" "$lbl"
# The llm-proxy starts on the run's internal network and is then connected
# to the default one, its way out.
"$C" run -d --name "ec-$$-c" $L --network "$NET" "$IMG" sleep 300 >/dev/null
if "$C" network connect bridge "ec-$$-c" >/dev/null 2>&1; then
  line PASS "network connect bridge (running)" ""
elif "$C" network connect "$DEFNET" "ec-$$-c" >/dev/null 2>&1; then
  line GAP "network connect bridge (running)" "no network named bridge; the default is $DEFNET, which the client connects instead (handled)"
else
  line FAIL "network connect (running)" "neither bridge nor $DEFNET"
fi
tail_all=all; [ "$KIND_CLI" = podman ] && tail_all=-1
"$C" logs --tail 5 "ec-$$-a" >/dev/null 2>&1 && "$C" logs --tail "$tail_all" "ec-$$-a" >/dev/null 2>&1 && line PASS "logs --tail N, --tail $tail_all" "" || line FAIL "logs --tail" ""
"$C" stop --timeout 1 "ec-$$-p" >/dev/null 2>&1 && "$C" rm -fv "ec-$$-p" >/dev/null && line PASS "stop --timeout, rm -fv" "" || line FAIL "stop, rm -fv" ""
cid=$("$C" create $L "$IMG" true) && "$C" cp "$cid:/etc/os-release" - | tar -tf - >/dev/null && "$C" rm -fv "$cid" >/dev/null && line PASS "create, cp ID:path -, rm" "" || line FAIL "create, cp, rm" ""
"$C" image inspect --format '{{json .Config.Labels}}' "$IMG" >/dev/null 2>&1 && line PASS "image inspect labels" "" || line FAIL "image inspect labels" ""

# --- Runtimes, uids, builds --------------------------------------------------
if "$C" run --rm $L --runtime agentry-no-such-runtime "$IMG" true >/dev/null 2>&1; then
  line GAP "--runtime nosuch" "accepted and run under the default: this API does not apply a named runtime (the runner holds a policy runtime on podman through a podman API service of the run's own, and checks each container after create)"
else
  line PASS "--runtime nosuch" "refused"
fi
mkdir -p "$WORK/out"
uid=$(id -u):$(id -g)
if "$C" run --rm $L --user "$uid" -v "$WORK/out:/out:z" "$IMG" touch /out/as-uid 2>/dev/null; then
  line PASS "--user $uid writes the person's dir" "owned by $(stat -c %u:%g "$WORK/out/as-uid")"
else
  owner=$("$C" run --rm $L --user 0 -v "$WORK/out:/out:z" "$IMG" sh -c 'touch /out/as-root' >/dev/null 2>&1 && stat -c %u:%g "$WORK/out/as-root")
  line MEASURED "--user $uid writes the person's dir" "denied (rootless=$ROOTLESS: the uid is a subordinate one); --user 0 writes it owned by ${owner:-nothing}"
fi
mkdir -p "$WORK/ctx" && printf 'FROM %s\nLABEL agentry.engine-check=%s\nRUN echo built > /built\n' "$IMG" "$TAG" > "$WORK/ctx/Dockerfile"
if out=$("$C" build -q -t "agentry-engine-check:$TAG" "$WORK/ctx" 2>&1) && "$C" image inspect "agentry-engine-check:$TAG" >/dev/null 2>&1; then
  line PASS "build -t (image in the store)" ""
else
  line GAP "build -t (image in the store)" "the image is not in the store: ${out##*$'\n'}"
fi

# --- The daemon socket from a container --------------------------------------
GW=${AGENTRY_GATEWAY_IMAGE:-$(awk '/^ *gateway:/ {print $2; exit}' "${AGENTRY_POLICY:-$HOME/.agentry/runner/policy.yaml}" 2>/dev/null)}
GW=${GW:-ghcr.io/blaktron/agentry-mcp-gateway:v0.43.3}
# The gateway image is pulled when the store lacks it, as agentry setup does.
"$C" image inspect "$GW" >/dev/null 2>&1 || "$C" pull -q "$GW" >/dev/null 2>&1 || true
if [ -S "$SOCK" ] && "$C" image inspect "$GW" >/dev/null 2>&1; then
  out=$("$C" run --rm $L --security-opt label=disable -v "$SOCK:/var/run/docker.sock" --entrypoint docker "$GW" version --format '{{.Server.Version}}' 2>&1) \
    && line PASS "socket in a container (label=disable)" "the gateway image's docker CLI reaches $out" || line FAIL "socket in a container" "$out"
else
  line SKIP "socket in a container" "no socket at $SOCK, or the gateway image $GW could not be pulled"
fi

# --- For a sandboxed run: measured, not judged -------------------------------
# The L4 relay binds 443 as a user with no capability: docker lets it by
# setting net.ipv4.ip_unprivileged_port_start=0 in every container.
BIND443='printf "start=%s " $(cat /proc/sys/net/ipv4/ip_unprivileged_port_start); (nc -l -p 443 >/dev/null 2>&1 &); sleep 1; netstat -ltn 2>/dev/null | grep -q ":443 " && echo bound || echo refused'
bind443() { # client and run args before the image
  "$@" --user 1000:1000 --cap-drop ALL "$IMG" sh -c "$BIND443" 2>&1 | tail -1
}
line MEASURED "443 as uid 1000, no capability" "$(bind443 "$C" run --rm $L)"
line MEASURED "  with the port sysctl at 0" "$(bind443 "$C" run --rm $L --sysctl net.ipv4.ip_unprivileged_port_start=0)"
line MEASURED "  created through the API socket" "$(DOCKER_HOST=unix://$SOCK bind443 docker run --rm $L)"
if [ -n "${RUNSC:-}" ] && [ -x "$RUNSC" ]; then
  if [ "$KIND_CLI" = podman ]; then
    # podman names a runtime by a path, not arguments: the gateway's runtime
    # is a wrapper that adds --host-uds=open.
    mkdir -p "$WORK/rt"
    printf '#!/bin/sh\nexec %s --ignore-cgroups "$@"\n' "$RUNSC" > "$WORK/rt/runsc"
    printf '#!/bin/sh\nexec %s --ignore-cgroups --host-uds=open "$@"\n' "$RUNSC" > "$WORK/rt/runsc-uds"
    chmod 755 "$WORK/rt/runsc" "$WORK/rt/runsc-uds"
    printf '[engine.runtimes]\nrunsc = ["%s"]\nrunsc-uds = ["%s"]\n' "$WORK/rt/runsc" "$WORK/rt/runsc-uds" > "$WORK/rt/containers.conf"
    RC=(env CONTAINERS_CONF_OVERRIDE="$WORK/rt/containers.conf" "$C")
    out=$("${RC[@]}" --runtime runsc info --format '{{.Host.OCIRuntime.Name}}' 2>&1 | tail -1)
    line MEASURED "podman --runtime runsc info" "$out"
    out=$("${RC[@]}" run --rm $L --runtime runsc "$IMG" sh -c 'dmesg 2>/dev/null | head -1; echo ran' 2>&1 | tr '\n' ' ' | cut -c1-200)
    line MEASURED "rootless run under runsc" "$out"
    out=$("${RC[@]}" run --rm $L --runtime runsc --security-opt label=disable "$IMG" sh -c 'dmesg 2>/dev/null | head -1; echo ran' 2>&1 | tr '\n' ' ' | cut -c1-200)
    line MEASURED "  with label=disable" "$out"
    line MEASURED "443 under runsc (label=disable)" "$(bind443 "${RC[@]}" run --rm $L --runtime runsc --security-opt label=disable)"
    line MEASURED "  with the port sysctl at 0" "$(bind443 "${RC[@]}" run --rm $L --runtime runsc --security-opt label=disable --sysctl net.ipv4.ip_unprivileged_port_start=0)"
    if "$C" image inspect "$GW" >/dev/null 2>&1; then
      for rt in runsc runsc-uds; do
        out=$("${RC[@]}" run --rm $L --runtime "$rt" --security-opt label=disable -v "$SOCK:/var/run/docker.sock" --entrypoint docker "$GW" version --format '{{.Server.Version}}' 2>&1 | tail -1 | cut -c1-200)
        line MEASURED "host socket from $rt" "$out"
      done
    fi
  else
    line SKIP "runsc measurements" "the docker daemon's runtimes are registered in its daemon.json, not here"
  fi
else
  line SKIP "runsc measurements" "RUNSC names no runsc binary"
fi

# --- One built-in run --------------------------------------------------------
POLICY=${AGENTRY_POLICY:-$HOME/.agentry/runner/policy.yaml}
KEY_ENV=$(awk '/^ *apiKeyEnv:/ {print $2; exit}' "$POLICY" 2>/dev/null)
if [ ! -f "$POLICY" ] || [ -z "$KEY_ENV" ] || [ -z "${!KEY_ENV:-}" ]; then
  line SKIP "built-in run" "no policy at $POLICY, or its model key (${KEY_ENV:-apiKeyEnv}) is not set"
else
  printf '# Container engines\n\nA container engine serves the Docker API: Docker Engine and podman are two.\n' > "$WORK/engines.md"
  (
    export AGENTRY_RUNNER_HOME="$WORK/home" AGENTRY_CONFIG_DIR="$WORK/config"
    cd "$WORK" && "$AGENTRY" knowledge create engine-check --dir "$WORK/pack" --file "$WORK/engines.md" --no-web --policy "$POLICY" --out "$WORK/run" >"$WORK/run.log" 2>&1
  )
  rc=$?
  eng=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["capabilities_enforced"].get("engine",""))' "$WORK/run/result.json" 2>/dev/null)
  refused=$(grep -c ' refuse ' "$WORK/run/receipts/docker-filter.log" 2>/dev/null)
  rid=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["runId"])' "$WORK/run/result.json" 2>/dev/null)
  left=$([ -n "$rid" ] && { "$C" ps -a -q --filter "label=agentry.run=$rid"; "$C" volume ls -q --filter "label=agentry.run=$rid"; "$C" network ls --format '{{.Name}}' | grep "^agentry-$rid"; })
  if [ $rc -eq 0 ] && [ -n "$eng" ] && [ "${refused:-1}" = 0 ] && [ -z "$left" ]; then
    line PASS "built-in run (Knowledge Creator)" "exit 0, filter refused nothing, nothing left; engine: $eng"
  else
    line FAIL "built-in run (Knowledge Creator)" "exit $rc, engine [$eng], filter refusals ${refused:-no log}, left [$(echo $left)]; $(tail -3 "$WORK/run.log" | tr '\n' ' ')"
  fi
fi

# --- A sandboxed run ---------------------------------------------------------
# A QuickAgent with declared egress and exec under the policy's runtime, with
# a scripted model: every container of the run must be under the runtime (the
# gateway under its --host-uds=open registration), the relays must have
# carried the egress, the receipt must say so, and nothing may be left.
SANDBOX_RT=""; SANDBOX_GW=""; SANDBOX_ENV=(env)
if [ "$KIND_CLI" = podman ] && [ -n "${RUNSC:-}" ] && [ -x "$RUNSC" ]; then
  SANDBOX_RT=runsc; SANDBOX_GW=runsc-uds; SANDBOX_ENV=(env CONTAINERS_CONF_OVERRIDE="$WORK/rt/containers.conf")
elif [ "$KIND_CLI" = docker ] && "$C" info --format '{{json .Runtimes}}' 2>/dev/null | python3 -c 'import json,sys; r=json.load(sys.stdin); sys.exit(0 if "runsc" in r and "runsc-uds" in r else 1)'; then
  SANDBOX_RT=runsc; SANDBOX_GW=runsc-uds
fi
if [ -z "$SANDBOX_RT" ]; then
  line SKIP "sandboxed run" "no runtime to run under (podman: RUNSC; docker: runsc and runsc-uds registered on the daemon)"
elif ! command -v python3 >/dev/null; then
  line SKIP "sandboxed run" "no python3 for the scripted model"
else
  # Where a container reaches this host: on a rootless podman the host's
  # own LAN address is the container namespace's (pasta copies it), so
  # podman's host.containers.internal entry is used; on docker the LAN
  # address, which the bridge routes to.
  LAN=$(ip route get 1 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="src") {print $(i+1); exit}}')
  LAN=${LAN:-$(hostname -I 2>/dev/null | awk '{print $1}')}
  if [ "$KIND_CLI" = podman ]; then
    HOSTIP=$("$C" run --rm $L "$IMG" getent hosts host.containers.internal 2>/dev/null | awk '{print $1; exit}')
    LAN=${HOSTIP:-$LAN}
  fi
  PORT=$((20000 + RANDOM % 20000))
  # The scripted model: OpenAI's chat-completions shape, streamed or not.
  # The first turn asks for the fetch tool on the declared host; once a tool
  # result is in the conversation it answers with that result's first line.
  cat > "$WORK/model.py" <<'PY'
import json, sys, http.server, time
PORT = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _json(self, code, body):
        b = json.dumps(body).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        self._json(200, {"object": "list", "data": [{"id": "stand-in", "object": "model"}]})
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        req = json.loads(self.rfile.read(n) or b"{}")
        msgs = req.get("messages", [])
        tools = [t.get("function", {}).get("name", "") for t in req.get("tools", [])]
        fetch = next((t for t in tools if t.endswith("fetch")), "")
        report = next((t for t in tools if t.endswith("write_report")), "")
        results = [m for m in msgs if m.get("role") == "tool"]
        def text_of(m):
            c = m.get("content")
            return " ".join(p.get("text", "") for p in c) if isinstance(c, list) else str(c)
        def call(name, args):
            return {"role": "assistant", "content": None, "tool_calls": [{"index": 0, "id": "call_%d" % (len(results) + 1), "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}]}, "tool_calls"
        if not results and fetch:
            delta, finish = call(fetch, {"url": "https://example.com/"})
        elif len(results) == 1 and report:
            page = text_of(results[0])
            title = next((l.strip() for l in page.splitlines() if "<title>" in l), page[:200])
            delta, finish = call(report, {"path": "title.md", "content": "# Fetched\n\n" + title + "\n"})
        else:
            last = text_of(results[-1]) if results else "no fetch tool offered"
            delta = {"role": "assistant", "content": "done: " + last[:200]}
            finish = "stop"
        with open(sys.argv[2], "a") as f: f.write("tools=%s roles=%s finish=%s\n" % (",".join(tools), ",".join(m.get("role", "") for m in msgs), finish))
        now = int(time.time())
        if req.get("stream"):
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.end_headers()
            for chunk in ({"id": "c1", "object": "chat.completion.chunk", "created": now, "model": "stand-in", "choices": [{"index": 0, "delta": delta, "finish_reason": None}]},
                          {"id": "c1", "object": "chat.completion.chunk", "created": now, "model": "stand-in", "choices": [{"index": 0, "delta": {}, "finish_reason": finish}], "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}):
                self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
        else:
            msg = dict(delta); msg.pop("content", None) if delta.get("content") is None else None
            self._json(200, {"id": "c1", "object": "chat.completion", "created": now, "model": "stand-in", "choices": [{"index": 0, "message": msg, "finish_reason": finish}], "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}})
http.server.ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
PY
  python3 "$WORK/model.py" "$PORT" "$WORK/model.log" & MODEL_PID=$!
  trap 'kill $MODEL_PID 2>/dev/null; cleanup' EXIT
  sleep 1
  # The upstream name servers, for the classes that resolve names under the
  # runtime (never a loopback stub, which the containers do not reach).
  RESOLVER=$( (resolvectl dns 2>/dev/null; grep nameserver /etc/resolv.conf 2>/dev/null) | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | grep -v '^127\.' | head -1)
  RESOLVER=${RESOLVER:-1.1.1.1}
  SB="$WORK/sandbox"; mkdir -p "$SB"
  (
    export AGENTRY_RUNNER_HOME="$SB/home" AGENTRY_CONFIG_DIR="$SB/config" ENGINE_CHECK_KEY=stand-in
    "${SANDBOX_ENV[@]}" "$AGENTRY" setup --yes --skip-images --provider custom --model stand-in --base-url "http://$LAN:$PORT/v1" --api-key-env ENGINE_CHECK_KEY --policy "$SB/policy.yaml" >"$SB/setup.log" 2>&1 || { cat "$SB/setup.log"; exit 1; }
    printf 'docker:\n  runtime: %s\n  gatewayRuntime: %s\n  resolvers: [%s]\n' "$SANDBOX_RT" "$SANDBOX_GW" "$RESOLVER" >> "$SB/policy.yaml"
    # The images under this engine, and the runner image for this CLI.
    "${SANDBOX_ENV[@]}" "$AGENTRY" setup --yes --provider custom --model stand-in --base-url "http://$LAN:$PORT/v1" --api-key-env ENGINE_CHECK_KEY --policy "$SB/policy.yaml" >>"$SB/setup.log" 2>&1 || { tail -5 "$SB/setup.log"; exit 1; }
  ) || line FAIL "sandboxed run: setup" "$(tail -3 "$SB/setup.log" 2>/dev/null | tr '\n' ' ')"
  if [ -f "$SB/policy.yaml" ] && grep -q "^  runtime: $SANDBOX_RT" "$SB/policy.yaml"; then
    ( export AGENTRY_RUNNER_HOME="$SB/home" AGENTRY_CONFIG_DIR="$SB/config" ENGINE_CHECK_KEY=stand-in
      echo "fetch the page and report its title" | "${SANDBOX_ENV[@]}" "$AGENTRY" run --local testdata/runner/sandbox-egress --policy "$SB/policy.yaml" \
        --allow-unsigned --yes --out "$SB/out" --output-format json --timeout 600 >"$SB/envelope.json" 2>"$SB/run.log" ) &
    SB_PID=$!
    # Every container of the run, with the runtime the engine gave it, as
    # long as the run is up.
    : > "$SB/runtimes.txt"
    while kill -0 "$SB_PID" 2>/dev/null; do
      ids=$("$C" ps -aq --filter label=agentry.run 2>/dev/null)
      if [ -n "$ids" ]; then
        if [ "$KIND_CLI" = podman ]; then
          "${SANDBOX_ENV[@]}" "$C" inspect --format '{{.Name}} {{.OCIRuntime}}' $ids 2>/dev/null >> "$SB/runtimes.txt"
        else
          "$C" inspect --format '{{.Name}} {{.HostConfig.Runtime}}' $ids 2>/dev/null >> "$SB/runtimes.txt"
        fi
      fi
      sleep 0.5
    done
    wait "$SB_PID"; rc=$?
    sort -u "$SB/runtimes.txt" | sed 's#^/##' > "$SB/runtimes-seen.txt"
    seen=$(wc -l < "$SB/runtimes-seen.txt")
    off=$(awk -v rt="$SANDBOX_RT" -v gw="$SANDBOX_GW" '($1 ~ /-gateway$/ && $2 != gw) || ($1 !~ /-gateway$/ && $2 != rt) {print $1"="$2}' "$SB/runtimes-seen.txt" | tr '\n' ' ')
    relays=$(grep -c 'docker-mcp-l[47]proxy' "$SB/runtimes-seen.txt")
    rtline=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["capabilities_enforced"].get("runtime",""))' "$SB/out/result.json" 2>/dev/null)
    refused=$(grep -c ' refuse ' "$SB/out/receipts/docker-filter.log" 2>/dev/null)
    verified=$(grep -c ' verified ' "$SB/out/receipts/docker-filter.log" 2>/dev/null)
    rid=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["runId"])' "$SB/out/result.json" 2>/dev/null)
    left=$([ -n "$rid" ] && { "$C" ps -a -q --filter "label=agentry.run=$rid"; "$C" volume ls -q --filter "label=agentry.run=$rid"; "$C" network ls --format '{{.Name}}' | grep "^agentry-$rid"; })
    svc=$(pgrep -f "system service --time=0 unix://.*$rid" 2>/dev/null | tr '\n' ' ')
    case "$rtline" in *"$SANDBOX_RT: created with"*) rtok=1;; *) rtok=0;; esac
    # The report the tools server wrote into the output folder is the
    # person's own: owned by them, so they can remove it (on a rootless
    # engine any other uid would be a subordinate one).
    owner=$(stat -c %u "$SB/out/reports/title.md" 2>/dev/null)
    if [ $rc -eq 0 ] && [ -z "$off" ] && [ "$seen" -ge 5 ] && [ "$relays" -ge 1 ] && [ $rtok = 1 ] && [ "${refused:-1}" = 0 ] && [ -z "$left" ] && [ -z "$svc" ] && [ "$owner" = "$(id -u)" ]; then
      line PASS "sandboxed run (egress + exec under $SANDBOX_RT)" "exit 0; $seen containers inspected live, every one under $SANDBOX_RT (gateway $SANDBOX_GW), $relays relay(s); filter verified ${verified:-0} create(s), refused nothing; the report is owned by uid $owner; nothing left"
    else
      line FAIL "sandboxed run (egress + exec under $SANDBOX_RT)" "exit $rc; off the runtime: [${off:-none}] of $seen seen, relays $relays, receipt names runtime: $rtok, filter refusals ${refused:-no log}, report owner [${owner:-no report}] want $(id -u), left [$(echo $left)], service still up [$svc]; $(tail -3 "$SB/run.log" | tr '\n' ' ')"
      sed 's/^/  seen: /' "$SB/runtimes-seen.txt"
    fi
    grep -q 'finish=stop' "$WORK/model.log" 2>/dev/null && line MEASURED "  the model's tool offer" "$(grep -m1 'finish=tool_calls' "$WORK/model.log" | cut -c1-200)"
  fi
  kill "$MODEL_PID" 2>/dev/null
fi

echo
if [ $FAILS -gt 0 ]; then
  echo "FAIL: $FAILS check(s) failed against $KIND_CLI"
  exit 1
fi
echo "ok: every check passed against $KIND_CLI (GAP and MEASURED lines are facts, not failures)"

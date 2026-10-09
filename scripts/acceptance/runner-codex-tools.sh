#!/usr/bin/env bash
# The codex adapter's gateway-only tools, measured on the installed Codex
# (adapter invariant 1; agentry-cli#737).
#
#   scripts/acceptance/runner-codex-tools.sh [model...]
#       (default: gpt-5.5 gpt-6-astra fixture-model)
#
# Codex decides part of its tool list from the model's catalog entry, not
# from feature keys: its bundled catalog gives real OpenAI models
# apply_patch, deferred tool_search and code mode, and a new release can add
# a built-in nothing switches off (0.162.0 added the goal tools). So the only
# proof is the request Codex actually sends. For each model this replays the
# adapter's rendered config.toml and models.json (the conformance golden,
# which the adapter's tests keep equal to what it compiles) against the
# installed `codex exec`, with a local fake Responses endpoint that records
# the request and a fake stdio MCP server standing in for the gateway, and
# requires the request's tools to be exactly the gateway's namespace and
# codex's three MCP resource readers (which read from that gateway).
#
# No model key, no network, no docker: the endpoint is 127.0.0.1 and
# answers 400 after recording. Run it on every Codex version bump.
set -euo pipefail
cd "$(dirname "$0")/../.."
command -v codex >/dev/null || { echo "SKIP: no codex on PATH" >&2; exit 0; }
GOLDEN=pkg/runner/adapter/testdata/conformance/codex/autonomous.json
MODELS=("$@"); [ ${#MODELS[@]} -gt 0 ] || MODELS=(gpt-5.5 gpt-6-astra fixture-model)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/codex-tools.XXXXXX")
trap 'kill "${SRV:-}" 2>/dev/null || true; rm -rf "$WORK"' EXIT
echo "codex: $(codex --version)"

cat > "$WORK/endpoint.py" <<'PY'
import http.server, sys
out = sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('content-length', 0)))
        with open(out, 'ab') as f: f.write(body + b"\n")
        self.send_response(400); self.send_header('content-type', 'application/json'); self.end_headers()
        self.wfile.write(b'{"error":{"message":"recorded","type":"invalid_request_error"}}')
    def do_GET(self): self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
s = http.server.HTTPServer(('127.0.0.1', 0), H)
open(sys.argv[1], 'w').write(str(s.server_address[1]))
s.serve_forever()
PY
cat > "$WORK/gateway.py" <<'PY'
import sys, json
def send(m): sys.stdout.write(json.dumps(m) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    try: m = json.loads(line)
    except ValueError: continue
    meth, i = m.get("method"), m.get("id")
    if meth == "initialize":
        send({"jsonrpc": "2.0", "id": i, "result": {"protocolVersion": m["params"].get("protocolVersion", "2025-06-18"), "capabilities": {"tools": {}}, "serverInfo": {"name": "gateway", "version": "0"}}})
    elif meth == "tools/list":
        send({"jsonrpc": "2.0", "id": i, "result": {"tools": [
            {"name": "report_write", "description": "write the report", "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}}},
            {"name": "inputs_read", "description": "read the inputs", "inputSchema": {"type": "object", "properties": {}}}]}})
    elif i is not None:
        send({"jsonrpc": "2.0", "id": i, "result": {}})
PY

fail=0
for model in "${MODELS[@]}"; do
  d="$WORK/$model"; mkdir -p "$d/home"
  : > "$d/requests.jsonl"
  python3 "$WORK/endpoint.py" "$d/port" "$d/requests.jsonl" & SRV=$!
  for _ in $(seq 50); do [ -s "$d/port" ] && break; sleep 0.1; done
  python3 - "$GOLDEN" "$model" "$(cat "$d/port")" "$d" "$WORK/gateway.py" <<'PY'
import json, sys
golden, model, port, d, gateway = sys.argv[1:]
files = json.load(open(golden))["files"]
cfg = files[".codex-home/config.toml"]
for old, new in [('model = "fixture-model"', f'model = "{model}"'),
                 ("@@RUN_CFG@@/harness/instruction.md", f"{d}/instruction.md"),
                 ("@@RUN_CFG@@/harness/.codex-home/models.json", f"{d}/models.json"),
                 ("https://model.example/v1", f"http://127.0.0.1:{port}/v1"),
                 ('url = "@@HOST_GATEWAY_URL@@"\nbearer_token_env_var = "MCP_GATEWAY_TOKEN"',
                  f'command = "python3"\nargs = ["{gateway}"]')]:
    if old not in cfg:
        sys.exit(f"the golden config no longer carries {old!r}; update this script")
    cfg = cfg.replace(old, new)
open(f"{d}/home/config.toml", "w").write(cfg)
catalog = json.loads(files[".codex-home/models.json"])
catalog["models"][0]["slug"] = catalog["models"][0]["display_name"] = model
json.dump(catalog, open(f"{d}/models.json", "w"))
open(f"{d}/instruction.md", "w").write("Do the thing.\n")
PY
  (cd "$d" && echo hi | env -i HOME="$HOME" PATH="$PATH" CODEX_HOME="$d/home" FIXTURE_API_KEY=not-a-key \
     timeout 60 codex exec --json --ephemeral --skip-git-repo-check --strict-config --ignore-rules --color never - \
     >"$d/stdout.jsonl" 2>"$d/stderr.txt") || true
  kill "$SRV" 2>/dev/null || true; wait "$SRV" 2>/dev/null || true
  if ! python3 - "$d" "$model" <<'PY'
import json, sys
d, model = sys.argv[1:]
lines = [l for l in open(f"{d}/requests.jsonl").read().splitlines() if l.strip()]
if not lines:
    print(f"FAIL {model}: codex sent no request"); print(open(f"{d}/stderr.txt").read()[-800:]); sys.exit(1)
req = json.loads(lines[0])
names = []
for t in req.get("tools", []):
    if t.get("type") == "namespace":
        names.append("namespace:" + t["name"] + "[" + ",".join(sorted(x["name"] for x in t.get("tools", []))) + "]")
    else:
        names.append(f'{t.get("type")}:{t.get("name")}')
want = sorted(["function:list_mcp_resources", "function:list_mcp_resource_templates",
               "function:read_mcp_resource", "namespace:mcp__gateway[inputs_read,report_write]"])
# The QuickAgent's instruction replaces codex's prompt: the request's
# instructions on 0.153.4, its first developer message on 0.162.0.
developer = [c.get("text", "") for it in req.get("input", []) if it.get("role") == "developer"
             for c in it.get("content") or []]
instruction = (req.get("instructions") or (developer[0] if developer else "")).strip()
if sorted(names) != want or instruction != "Do the thing.":
    print(f"FAIL {model}: tools {sorted(names)}, instruction {instruction[:80]!r}"); sys.exit(1)
print(f"ok   {model}: the gateway's tools and the three resource readers, nothing else")
PY
  then fail=1; fi
done
exit $fail

#!/usr/bin/env bash
# The claude and codex adapters hand an input tool's answer to the model.
#
#   scripts/acceptance/runner-input-resource.sh
#
# read_input and search_inputs answer a person's input as an MCP embedded
# text resource, so the gateway's secret blocking passes it. Whether a host harness gives the model that resource's text, and
# whether it scrubs secret-shaped strings from it, is the harness's own
# behaviour, so the only proof is the request it actually sends. For each
# installed harness this replays the adapter's conformance golden (its argv
# and MCP config for claude, its config.toml for codex) against a local fake
# model endpoint that answers the first request with a call to the
# gateway's read_input and records the next, and a fake stdio MCP server
# standing in for the gateway, whose read_input answers an embedded
# resource carrying a planted, secret-shaped line. It requires that line,
# unchanged, in what the harness sends the model as the tool's result.
#
# No model key, no network, no docker: both endpoints are 127.0.0.1. Run it
# on every claude or codex harness version bump (CLAUDE and CODEX name the
# binaries; default: claude and codex on PATH).
set -euo pipefail
cd "$(dirname "$0")/../.."
command -v python3 >/dev/null || { echo "SKIP: no python3 (the fakes are python)" >&2; exit 0; }
CLAUDE=${CLAUDE:-claude}
CODEX=${CODEX:-codex}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/input-resource.XXXXXX")
trap 'kill "${SRV:-}" 2>/dev/null || true; rm -rf "$WORK"' EXIT
PLANT="AKIAZZZZZZZZZZZZZZZZ planted-input-line sk-ant-api03-$(printf 'x%.0s' $(seq 40))"

cat > "$WORK/gateway.py" <<'PY'
import json, os, sys
plant = os.environ["PLANT"]
def send(m): sys.stdout.write(json.dumps(m) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    try: msg = json.loads(line)
    except Exception: continue
    i, m = msg.get("id"), msg.get("method")
    if m == "initialize":
        send({"jsonrpc": "2.0", "id": i, "result": {"protocolVersion": msg["params"].get("protocolVersion", "2025-06-18"), "capabilities": {"tools": {}}, "serverInfo": {"name": "gateway", "version": "0"}}})
    elif m == "tools/list":
        send({"jsonrpc": "2.0", "id": i, "result": {"tools": [{"name": "read_input", "description": "read an input file", "inputSchema": {"type": "object", "properties": {"path": {"type": "string"}}}}]}})
    elif m == "tools/call":
        send({"jsonrpc": "2.0", "id": i, "result": {"content": [{"type": "resource", "resource": {"uri": "agentry-input:///notes.txt", "mimeType": "text/plain", "text": plant}}]}})
    elif i is not None:
        send({"jsonrpc": "2.0", "id": i, "result": {}})
PY

cat > "$WORK/model.py" <<'PY'
# kind is "anthropic" (Messages) or "responses" (OpenAI Responses): the
# first request that offers read_input gets a streamed call to it; every
# request is recorded.
import http.server, json, sys
kind, out, portfile = sys.argv[1:4]
called = [False]
def sse(h, events):
    h.send_response(200); h.send_header("content-type", "text/event-stream"); h.end_headers()
    for name, data in events:
        h.wfile.write(f"event: {name}\ndata: {json.dumps(data)}\n\n".encode())
def anthropic(req):
    tool = next((t["name"] for t in req.get("tools", []) if t.get("name", "").endswith("read_input")), None)
    msg = {"id": "msg_1", "type": "message", "role": "assistant", "model": req.get("model", "m"), "content": [], "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 1, "output_tokens": 1}}
    if tool and not called[0]:
        called[0] = True
        return [("message_start", {"type": "message_start", "message": msg}),
                ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "tool_use", "id": "toolu_1", "name": tool, "input": {}}}),
                ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "input_json_delta", "partial_json": "{\"path\": \"notes.txt\"}"}}),
                ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "tool_use", "stop_sequence": None}, "usage": {"output_tokens": 5}}),
                ("message_stop", {"type": "message_stop"})]
    return [("message_start", {"type": "message_start", "message": msg}),
            ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "done"}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 1}}),
            ("message_stop", {"type": "message_stop"})]
def responses(req):
    resp = {"id": "resp_1", "object": "response", "status": "in_progress", "output": []}
    if not called[0]:
        called[0] = True
        item = {"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read_input", "namespace": "mcp__gateway", "arguments": "{\"path\": \"notes.txt\"}", "status": "completed"}
    else:
        item = {"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "done", "annotations": []}]}
    done = dict(resp, status="completed", output=[item], usage={"input_tokens": 1, "output_tokens": 1, "total_tokens": 2, "input_tokens_details": {"cached_tokens": 0}, "output_tokens_details": {"reasoning_tokens": 0}})
    return [(e["type"], e) for e in [{"type": "response.created", "response": resp},
            {"type": "response.output_item.added", "output_index": 0, "item": item},
            {"type": "response.output_item.done", "output_index": 0, "item": item},
            {"type": "response.completed", "response": done}]]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        with open(out, "ab") as f: f.write(body + b"\n")
        if "count_tokens" in self.path:
            self.send_response(200); self.send_header("content-type", "application/json"); self.end_headers()
            self.wfile.write(b'{"input_tokens": 10}'); return
        try: req = json.loads(body)
        except Exception: req = {}
        sse(self, anthropic(req) if kind == "anthropic" else responses(req))
    def do_GET(self): self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
open(portfile, "w").write(str(s.server_address[1]))
s.serve_forever()
PY

start_model() { # kind dir
  : > "$2/requests.jsonl"
  python3 "$WORK/model.py" "$1" "$2/requests.jsonl" "$2/port" & SRV=$!
  for _ in $(seq 50); do [ -s "$2/port" ] && break; sleep 0.1; done
  [ -s "$2/port" ] || { echo "FAIL $1: the fake endpoint did not start" >&2; return 1; }
}
# carried <dir> <name>: the planted line, unchanged, in a request after the
# tool call (what the model was handed as the tool's result).
carried() {
  python3 - "$1/requests.jsonl" "$2" <<'PY'
import json, os, sys
path, name = sys.argv[1:]
plant = os.environ["PLANT"]
reqs = [json.loads(l) for l in open(path).read().splitlines() if l.strip()]
if len(reqs) < 2:
    print(f"FAIL {name}: the harness made {len(reqs)} request(s); it never sent the tool's result back", file=sys.stderr); sys.exit(1)
later = json.dumps(reqs[1:])
if plant not in later:
    print(f"FAIL {name}: the input's text is not in what the model was handed after read_input", file=sys.stderr); sys.exit(1)
print(f"ok   {name}: the model was handed the input's text, secret-shaped line unchanged")
PY
}
export PLANT
fail=0

if command -v "$CLAUDE" >/dev/null; then
  d="$WORK/claude"; mkdir -p "$d/home"
  echo "claude: $("$CLAUDE" --version)"
  if start_model anthropic "$d"; then
    python3 - pkg/runner/adapter/testdata/conformance/claude/autonomous.json "$d" "$WORK/gateway.py" <<'PY'
import json, sys
golden, d, gateway = sys.argv[1:]
g = json.load(open(golden))
open(f"{d}/instruction.md", "w").write("Read notes.txt with read_input, then say done.\n")
mcp = json.loads(g["files"]["mcp.json"])
mcp["mcpServers"]["gateway"] = {"type": "stdio", "command": "python3", "args": [gateway]}
json.dump(mcp, open(f"{d}/mcp.json", "w"))
argv = g["host"]["argv"]
open(f"{d}/argv", "w").write("\n".join(argv) + "\n")
PY
    mapfile -t argv < "$d/argv"
    (cd "$d" && echo "read notes.txt" | env -i HOME="$d/home" PATH="$PATH" PLANT="$PLANT" ANTHROPIC_BASE_URL="http://127.0.0.1:$(cat "$d/port")" ANTHROPIC_AUTH_TOKEN=not-a-key \
       CLAUDE_CONFIG_DIR="$d/home/.claude" CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 DISABLE_TELEMETRY=1 DISABLE_AUTOUPDATER=1 \
       timeout 90 "$CLAUDE" "${argv[@]}" >"$d/stdout.jsonl" 2>"$d/stderr.txt") || true
    kill "$SRV" 2>/dev/null || true; wait "$SRV" 2>/dev/null || true
    carried "$d" claude || fail=1
  else fail=1; fi
else echo "SKIP claude: not on PATH" >&2; fi

if command -v "$CODEX" >/dev/null; then
  d="$WORK/codex"; mkdir -p "$d/home"
  echo "codex: $("$CODEX" --version)"
  if start_model responses "$d"; then
    python3 - pkg/runner/adapter/testdata/conformance/codex/autonomous.json "$(cat "$d/port")" "$d" "$WORK/gateway.py" <<'PY'
import json, sys
golden, port, d, gateway = sys.argv[1:]
files = json.load(open(golden))["files"]
cfg = files[".codex-home/config.toml"]
for old, new in [("@@RUN_CFG@@/harness/instruction.md", f"{d}/instruction.md"),
                 ("@@RUN_CFG@@/harness/.codex-home/models.json", f"{d}/models.json"),
                 ("https://model.example/v1", f"http://127.0.0.1:{port}/v1"),
                 ('url = "@@HOST_GATEWAY_URL@@"\nbearer_token_env_var = "MCP_GATEWAY_TOKEN"', f'command = "python3"\nargs = ["{gateway}"]\nenv_vars = ["PLANT"]')]:
    if old not in cfg:
        sys.exit(f"the golden config no longer carries {old!r}; update this script")
    cfg = cfg.replace(old, new)
open(f"{d}/home/config.toml", "w").write(cfg)
open(f"{d}/models.json", "w").write(files[".codex-home/models.json"])
open(f"{d}/instruction.md", "w").write("Read notes.txt with read_input, then say done.\n")
PY
    (cd "$d" && echo "read notes.txt" | env -i HOME="$d/fakehome" PATH="$PATH" PLANT="$PLANT" CODEX_HOME="$d/home" FIXTURE_API_KEY=not-a-key \
       timeout 90 "$CODEX" exec --json --ephemeral --skip-git-repo-check --strict-config --ignore-rules --color never - \
       >"$d/stdout.jsonl" 2>"$d/stderr.txt") || true
    kill "$SRV" 2>/dev/null || true; wait "$SRV" 2>/dev/null || true
    carried "$d" codex || fail=1
  else fail=1; fi
else echo "SKIP codex: not on PATH" >&2; fi

exit $fail

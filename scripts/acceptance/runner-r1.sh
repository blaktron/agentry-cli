#!/usr/bin/env bash
# Tool-plane acceptance: drive the companion tools server over real MCP
# stdio, the same code path the gateway containerizes, with no docker. Proves: granted-tools-only listing, report writing,
# workspace reads, grant enforcement, and traversal refusal.
set -euo pipefail
cd "$(dirname "$0")/../.."

AGENTRY=${AGENTRY:-bin/agentry}
[ -x "$AGENTRY" ] || make build >/dev/null

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
# The consent records `agentry run` writes go to the throwaway config dir,
# not the person's ~/.config/agentry/installed.yaml.
export AGENTRY_CONFIG_DIR="$WORK/config"
mkdir -p "$WORK/ws" "$WORK/out"
echo "workspace note" >"$WORK/ws/notes.txt"

fail() { echo "FAIL: $*" >&2; exit 1; }

# The tools server refuses any spec version but its own, so the specs below
# take it from the source rather than a number that ages.
SPEC_VERSION=$(sed -n 's/^const SpecVersion = "\([0-9]*\)"$/\1/p' runnerimage/toolsrv/spec.go)
[ -n "$SPEC_VERSION" ] || fail "cannot read toolsrv.SpecVersion"

# A read-only spec: fs_read/fs_list + write_report and nothing else.
mkdir -p "$WORK/in"
echo "provided input" >"$WORK/in/brief.txt"

cat >"$WORK/spec.json" <<EOF
{
  "specVersion": "$SPEC_VERSION",
  "workspaceDir": "$WORK/ws",
  "outDir": "$WORK/out",
  "inputsDir": "$WORK/in",
  "fsRead": { "enabled": true },
  "fsWrite": { "enabled": false },
  "fetch": { "enabled": false, "allowAll": false },
  "exec": {},
  "limits": {}
}
EOF

mcp() { # requests on stdin -> responses on stdout
  "$AGENTRY" runner-tools --spec "$WORK/spec.json" 2>/dev/null
}

REQS='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"r1","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fs_read","arguments":{"path":"notes.txt"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fs_read","arguments":{"path":"../escape"}}}
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"write_report","arguments":{"path":"r1.md","content":"report body"}}}
{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"fs_write","arguments":{"path":"x","content":"y"}}}
{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_inputs","arguments":{}}}
{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"read_input","arguments":{"path":"brief.txt"}}}
{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"read_input","arguments":{"path":"../ws/notes.txt"}}}
{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"fs_read","arguments":{"path":"notes.txt","maxBytes":"9"}}}'

OUT=$(printf '%s\n' "$REQS" | mcp)

echo "$OUT" | grep -q '"agentry-runner-tools"'   || fail "initialize: serverInfo missing"
echo "$OUT" | grep -q '"fs_read"'                 || fail "tools/list: fs_read missing"
echo "$OUT" | grep -q '"write_report"'            || fail "tools/list: write_report missing"
echo "$OUT" | grep -q '"list_inputs"'             || fail "tools/list: list_inputs missing"
echo "$OUT" | grep -q '"read_input"'              || fail "tools/list: read_input missing"
echo "$OUT" | grep -q '"fs_write"' && fail "tools/list: undeclared fs_write present"
echo "$OUT" | grep -q 'workspace note'            || fail "fs_read: content missing"
LINE4=$(echo "$OUT" | grep '"id":4')
echo "$LINE4" | grep -q '"isError":true'          || fail "traversal not refused: $LINE4"
[ -f "$WORK/out/reports/r1.md" ]                  || fail "write_report did not land in out/reports"
grep -q "report body" "$WORK/out/reports/r1.md"   || fail "report content wrong"
LINE6=$(echo "$OUT" | grep '"id":6')
echo "$LINE6" | grep -qi 'error'                  || fail "calling an ungranted tool must error: $LINE6"
echo "$OUT" | grep -q 'brief.txt'                 || fail "list_inputs: provided file missing"
echo "$OUT" | grep -q 'provided input'            || fail "read_input: content missing"
LINE9=$(echo "$OUT" | grep '"id":9')
echo "$LINE9" | grep -q '"isError":true'          || fail "read_input traversal not refused: $LINE9"
# A number sent as a JSON string is the number it encodes.
LINE10=$(echo "$OUT" | grep '"id":10')
echo "$LINE10" | grep -q '"isError":true' && fail "stringified maxBytes refused: $LINE10"
echo "$LINE10" | grep -q 'continue with offset 9]' || fail "stringified maxBytes 9 did not cap the read: $LINE10"

# A catalogued exec command must not read the tools
# server's environment, which holds every granted secret. The server starts
# with a planted secret in its environment block — what /proc/<pid>/environ
# shows — and a declared `sh` tries to read it from its parent (the server).
# A caller holding CAP_SYS_PTRACE (a privileged container) can read any
# non-dumpable process; the gateway's tool containers do not hold it, so the
# check is skipped, and says so, only there. Off Linux there is no /proc.
if [ -r /proc/self/status ]; then
  CAPEFF=$(awk '/^CapEff:/{print $2}' /proc/self/status)
  if (( 0x$CAPEFF & 0x80000 )); then
    echo "runner-r1: exec environ check skipped (this shell holds CAP_SYS_PTRACE; the tool containers do not)"
  else
    SECRET="r1-planted-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
    cat >"$WORK/spec-exec.json" <<EOF
{
  "specVersion": "$SPEC_VERSION",
  "workspaceDir": "$WORK/ws",
  "outDir": "$WORK/out",
  "fsRead": { "enabled": false },
  "fsWrite": { "enabled": false },
  "fetch": { "enabled": false, "allowAll": false, "secretEnv": ["R1_GRANTED_SECRET"] },
  "exec": { "commands": ["sh", "cat"] },
  "limits": {}
}
EOF
    EREQS='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"r1","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run_command","arguments":{"command":"sh","args":["-c","cat /proc/$PPID/environ; echo; echo probed=$PPID"]}}}'
    EOUT=$(printf '%s\n' "$EREQS" | R1_GRANTED_SECRET="$SECRET" "$AGENTRY" runner-tools --spec "$WORK/spec-exec.json" 2>/dev/null)
    ELINE=$(echo "$EOUT" | grep '"id":2')
    echo "$ELINE" | grep -q 'probed='            || fail "exec environ probe did not run: $ELINE"
    echo "$EOUT" | grep -q "$SECRET"             && fail "an exec child read the tools server's environment: $ELINE"
    echo "$ELINE" | grep -q 'Permission denied'  || fail "reading the tools server's environ was not refused: $ELINE"
    echo "runner-r1: exec cannot read the tools server's environment"
  fi
fi

echo "runner-r1: all checks passed"

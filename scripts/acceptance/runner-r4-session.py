#!/usr/bin/env python3
"""M-R4 interactive acceptance (plan runner-m-r4-adapters §8, decision D3:
an adapter is done when BOTH modes pass).

    scripts/acceptance/runner-r4-session.py <adapter> [out-dir]

Drives one interactive BriefAgent session end to end and then checks the
run's own record rather than the screen: the envelope is ok and says which
mode, adapter and harness ran; the agent answered from what its gateway
tools read out of the workspace; and the transcript and event receipts
exist. An interactive run insists on a terminal (the chat is the input), so
this allocates a pty — which is also the only way to exercise the TUI, the
session driver and the harness together.

Needs the same things as runner-r4.sh: a docker daemon, the operator policy
and the key it names, plus the adapter's harness installed.
"""

import atexit
import json
import os
import pty
import re
import select
import shutil
import subprocess
import sys
import tempfile
import time

if len(sys.argv) < 2:
    print(__doc__); sys.exit(2)
adapter = sys.argv[1]
out = sys.argv[2] if len(sys.argv) > 2 else f"/tmp/agentry-r4-session-{adapter}"
shutil.rmtree(out, ignore_errors=True)
argv = ["bin/agentry", "run", "--local", "testdata/runner/hello-chat",
        "--mode", "interactive", "--adapter", adapter,
        "--allow-unsigned", "--yes", "--out", out]
m, s = pty.openpty()
os.set_blocking(m, False)
# The consent record `agentry run` writes (§R1) goes to a throwaway config
# dir, not the person's ~/.config/agentry/installed.yaml (agentry-cli#101).
config_dir = tempfile.mkdtemp(prefix="agentry-r4-config-")
atexit.register(shutil.rmtree, config_dir, True)
env = dict(os.environ, AGENTRY_CONFIG_DIR=config_dir)
p = subprocess.Popen(argv, stdin=s, stdout=s, stderr=s, close_fds=True, env=env)
os.close(s)
buf = bytearray()
ANSI = re.compile(rb"\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][B0]|\x1b[=>]|\r")

def pump(seconds):
    end = time.time() + seconds
    while time.time() < end:
        r, _, _ = select.select([m], [], [], 0.2)
        if r:
            try:
                chunk = os.read(m, 65536)
            except OSError:
                return
            if not chunk:
                return
            buf.extend(chunk)

def plain():
    return ANSI.sub(b"", bytes(buf)).decode("utf-8", "replace")

def send(text):
    os.write(m, text.encode() + b"\r")

pump(25)                       # sandbox comes up, opening line renders
send("what does the note say?")
pump(90)                       # the harness reads the workspace and answers
send("/finish")
pump(60)                       # the session closes and the result is written
try:
    code = p.wait(timeout=60)
except subprocess.TimeoutExpired:
    p.kill(); code = -1
os.close(m)
screen = plain()
open(out + ".pty.log", "w").write(screen)

ok = True
def check(label, cond, detail=""):
    global ok
    print(("PASS " if cond else "FAIL ") + label + (("  " + detail) if detail and not cond else ""))
    ok = ok and cond

check("the CLI exited cleanly", code == 0, f"exit {code}")
env_path = os.path.join(out, "result.json")
check("a result envelope was written", os.path.exists(env_path))
if os.path.exists(env_path):
    env = json.load(open(env_path))
    check("the session's envelope is ok", env.get("status") == "ok", str(env.get("status")))
    check("the envelope records the interactive mode", env.get("harness", {}).get("mode") == "interactive")
    check("the envelope records the adapter", env.get("harness", {}).get("adapter") == adapter)
    if adapter == "claude":
        check("the envelope names the host harness that ran",
              "claude" in (env.get("harness", {}).get("host") or ""), str(env.get("harness", {}).get("host")))
    result = json.dumps(env.get("result", ""))
    check("the agent answered with what it read from the workspace",
          "hello from the workspace" in result, result[:200])
tr = os.path.join(out, "receipts", "transcript.md")
check("a transcript receipt exists", os.path.exists(tr))
if os.path.exists(tr):
    body = open(tr).read()
    check("the transcript holds the user turn", "what does the note say?" in body)
    check("the transcript records a tool call", "tool" in body.lower(), body[:300])
ev = os.path.join(out, "receipts", "events.ndjson")
check("an events receipt exists", os.path.exists(ev))
# The session store is a per-run volume, gone with the run (agentry-cli#90);
# no world-writable host directory stands in for it.
vols = subprocess.run(["docker", "volume", "ls", "--filter", "label=agentry.run", "--format", "{{.Name}}"],
                      capture_output=True, text=True).stdout.split()
check("no session volume survives the run", not vols, " ".join(vols))
print("SESSION_CHECK_EXIT=" + ("0" if ok else "1"))
sys.exit(0 if ok else 1)

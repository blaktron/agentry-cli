#!/usr/bin/env python3
"""Catalogue retained test evidence; --check catches unrecorded additions/removals."""
import argparse
import difflib
import json
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
OUTPUT = ROOT / "docs/testing-inventory.json"

# Each package's behavior owner. These describe ownership, not expected
# outcomes: whether two tests are equivalent is decided by reading their
# assertions, never inferred from this catalogue.
PURPOSES = {
    "cmd/agentry": "The agentry command surface: commands, flags, JSON output, exit codes and their wiring to the runner and the registry",
    "internal/highlights": "What each release of the agentry CLI brings, in a few plain lines for the person using it",
    "internal/origin": "Compares URLs by origin: scheme, host and port",
    "internal/pastruns": "Reads the runs a person has made from their output folders: every folder directly under an output",
    "internal/scriptedmodel": "A model server for tests: it plays a script of replies and failures as the Anthropic Messages API and as OpenAI-style Chat Completions, and records every request it receives",
    "internal/storefile": "The file primitives the stores under the runner home share (the credential stores of pkg/runner/connection",
    "internal/version": "Carries the CLI's build-stamped version",
    "pkg/authflow": "The client half of browser sign-in: a one-shot loopback listener that receives a single-use CODE from the",
    "pkg/browsersignin": "Lets a person sign in to a web site in a browser window and hands back the cookies that browser would send",
    "pkg/configdir": "Resolves the agentry client configuration directory (default ~/.config/agentry, relocated by",
    "pkg/dirpublish": "Replaces a directory whole: the new tree is built in a staging directory beside the destination and",
    "pkg/drafter": "The Drafter: the QuickAgent package that turns a person's description into a draft for the Builder",
    "pkg/examiner": "The Examiner: the built-in QuickAgent that reviews a QuickAgent a person has built and reports what the",
    "pkg/export": "The interop exporters: pure functions of (archive, manifest, target) that emit the claude-skill and",
    "pkg/installed": "The shared install-state record and the capability diff: an install shows the capability summary and the",
    "pkg/knowledge": "The knowledge pack: an LLM wiki — compiled markdown pages with an index — together with the documents it",
    "pkg/knowledge/build": "Makes or extends a knowledge pack with the Knowledge Creator: it stages the documents, decodes them before",
    "pkg/knowledge/creator": "The Knowledge Creator: the QuickAgent package that compiles documents into a knowledge pack, embedded in",
    "pkg/knowledge/remote": "The client of a registry's knowledge routes: wrap and unwrap a pack's data key, create a pack, upload and",
    "pkg/knowledge/ship": "Lays a knowledge pack into a QuickAgent's package so the release carries it: the pack's files under",
    "pkg/knowledge/store": "The local knowledge store: the knowledge packs a person keeps on this machine, sealed, under the runner",
    "pkg/mcpauth": "Signs a person in to a remote MCP server by OAuth, as the MCP authorization specification (2026-07-28)",
    "pkg/pathguard": "The refusal every recursive delete of a derived directory passes, and the containment check every write at",
    "pkg/persistent/assistant": "The Assistant: the built-in persistent agent that is the person's own assistant, embedded in the CLI and",
    "pkg/persistent/offer": "The shape of an MCP server a persistent built-in comes with: an offered server, named in the built-in's",
    "pkg/plainpath": "Checks a path under an os.Root before a tree that a container wrote is read: every part of the path is",
    "pkg/removal": "Deletes what a run or a build leaves that must not stay on disk (a run's secrets file, a knowledge pack's",
    "pkg/reportdesigner": "The Report Designer: the QuickAgent package that designs a person's report design, the look of their",
    "pkg/reportdesigner/build": "Runs the Report Designer: it stages what the person gave it (the design to start from without its",
    "pkg/reportthumbs": "A thumbnail of each built-in report theme: the first page of one sample report",
    "pkg/runhost": "What a remote runhost is made of on both sides of the SSH connection that reaches it: where things live on",
    "pkg/runhost/clirelease": "Decides whether a CLI binary is one of Agentry's signed releases: SHA256SUMS signed with an update-signing",
    "pkg/runhost/job": "The document a device sends a remote runhost to start one run: the release it verified, what it decided",
    "pkg/runhost/output": "A remote run's output at rest on the runhost: the run's output folder packed into one tar of the members a",
    "pkg/runhost/payload": "Carries the files a run on a remote runhost takes from the device: its inputs, the files that answer its",
    "pkg/runhost/remote": "The device side of a remote run: a job's session, schedule, update, credits and output collection over SSH",
    "pkg/runhost/setup": "Makes a Linux server the person owns into a remote runhost, over SSH, and takes it away again",
    "pkg/runhost/signin": "A model sign-in made for a remote runhost: Claude Code's own sign-in, Sign in with ChatGPT and Sign in",
    "pkg/runhost/store": "The runhost's store of the releases it has run: each one's archive and the facts it arrived with",
    "pkg/runner/acpagent": "The agent side of the Agent Client Protocol: it lets an editor or an orchestrating agent drive one",
    "pkg/runner/adapter": "The harness adapter interface and its registry",
    "pkg/runner/browser": "What the runner knows of the browser server, the gateway-launched container a release that declares",
    "pkg/runner/clamdb": "This machine's copy of the ClamAV signature bundle the decode step checks every input against",
    "pkg/runner/claudesignin": "\"Use your Claude subscription (runs Claude Code)\": Claude Code's own sign-in into a config directory of",
    "pkg/runner/compile": "The capability compiler: it turns manifest + operator policy into the run plan — generated gateway file",
    "pkg/runner/connection": "A person's OAuth connections to remote MCP servers on this machine: one per QuickAgent release (ref plus",
    "pkg/runner/copilotmodels": "Lists the models a person's Copilot plan offers for `agentry setup` and the desktop's model setup, so",
    "pkg/runner/decode": "The runner's decode step: the documents a run is given are turned into plain text before any agent starts",
    "pkg/runner/deliver": "Copies a run's deliverables out of its output folder to a folder the person chose",
    "pkg/runner/destination": "Where the person keeps the places a run's deliverables may be sent",
    "pkg/runner/desktop": "How a run reaches Agentry Desktop's link service: the socket, the bind, call and unbind messages, and the server side a stand-in or the app implements",
    "pkg/runner/dockerx": "Wraps the docker CLI behind the smallest interface the engine needs",
    "pkg/runner/engine": "Executes a compiled run plan: render the plan's placeholder-bearing files into a per-run directory",
    "pkg/runner/gateway": "Owns the \"installed on first agent use\" half: making sure the pinned docker-mcp-gateway and harness images",
    "pkg/runner/harnessbuild": "Makes the harness images that are built on this machine from a vendor artifact and never published",
    "pkg/runner/harnessprobe": "Describes the host-run harness behind an adapter name on this machine: whether its executable resolves",
    "pkg/runner/hostpin": "Pins the hosts a proxy may reach to the addresses this machine resolves for them, as --add-host entries",
    "pkg/runner/hybrid": "Looks up a run's risky inputs on CrowdStrike's Hybrid Analysis by SHA-256 alone",
    "pkg/runner/imagelabel": "Names the label every image the runner builds carries: org.agentry.image=<kind>. A build's tag is",
    "pkg/runner/inference": "The client of a registry's credits sessions: the sessions a run, or a persistent agent, pays for its model",
    "pkg/runner/installdir": "The runner home's per-release directory: where a verified release is laid down for runs, and where",
    "pkg/runner/intake": "Collects a QuickAgent's up-front questions (manifest v2 `intake`): asked before the sandbox starts, in",
    "pkg/runner/localinstall": "Installs a QuickAgent from a folder on this machine that was never published, into the runner home, where",
    "pkg/runner/localrun": "What a run needs besides the registry: the plan's reuse or compile for any run, a release's or not",
    "pkg/runner/mcpinstall": "Resolves and installs the MCP servers a QuickAgent declares: resolution turns each manifest pin into the",
    "pkg/runner/modelcheck": "A saved model checked before it is saved: its provider reached with the credential it names, and the model",
    "pkg/runner/modelkey": "A model provider's API key in the OS keychain — Keychain on macOS, Credential Manager on Windows, the",
    "pkg/runner/modelsignin": "A person's model sign-ins on this machine: the grants that let their local runs spend a model vendor's",
    "pkg/runner/osvdb": "This machine's copy of OSV's vulnerability databases, which the Coding agent's dependency scan reads offline",
    "pkg/runner/owner": "Labels the docker objects a run creates with the process that created them, and tells a live owner from a",
    "pkg/runner/persistent": "Persistent agents: the built-ins the runner keeps between a person's conversations, their accounts, memory and upkeep",
    "pkg/runner/policy": "Loads the operator policy — the machine-side half of every run plan",
    "pkg/runner/procx": "What every process the runner starts has in common — one place, so a new spawn site has one call to make",
    "pkg/runner/profile": "Stores a QuickAgent's MCP profile: the persistent, credentialed half of its gateway configuration, keyed",
    "pkg/runner/receipt": "The run result envelope — the runtime counterpart of the registry's public scan report",
    "pkg/runner/session/protocol": "The session protocol: an interactive session driven by a program rather than a person, over pipes and with",
    "pkg/runner/savedmodels": "The rules a client applies to the saved models and providers: what names a model, a removal refused until",
    "pkg/runner/setup": "The operator-experience flows: first-run machine setup with LLM-provider configuration, the guided",
    "pkg/runner/skill": "Reads Agent Skills folders: a SKILL.md whose frontmatter names the skill and says when to use it, and the",
    "pkg/runner/sources": "The input-source seam",
    "pkg/runner/spec": "The run plan — the deterministic artifact the capability compiler produces at install time and the engine",
    "pkg/runner/targethost": "Readies a run's target hosts before anything starts: a target host is a machine the person names when a",
    "pkg/runner/unsafeexec": "The data side of Unsafe Mode: the unsafe base image's command index, extras list and ambiguous list, read",
    "pkg/schedule": "The schedules of runs on this computer: when a QuickAgent runs again, the store that is the truth about",
    "pkg/termtext": "Prepares author text (a release guide's overview and key help, the Drafter's narration) for a terminal",
    "pkg/managed": "A client's copy of an organization's managed settings: fetched with the account's token, verified against the",
    "pkg/trust": "The shared client-side provenance layer: fetch a registry's signing keys, pin them on first use (TOFU)",
    "pkg/tui": "The terminal chat for an interactive run: one full-screen conversation over a session.Session — transcript",
    "pkg/validate": "`agentry validate`'s workspace check: offline parity with the server's submit validation — every manifest",
    "pkg/voice": "The Writing Voice: the instructions an LLM follows to write as a person writes, one Markdown file",
    "pkg/voice/build": "Writes a Writing Voice with the Voice Writer: it stages the person's writing samples, decodes them before",
    "pkg/voice/remote": "The client of a registry's Writing Voice routes (the core's docs/voice.md): read the account's voice, put",
    "pkg/voice/writer": "The Voice Writer: the QuickAgent package that writes a person's Writing Voice from samples of their",
    "runnerimage": "The embedded build context of the locally built agentry-runner image",
    "runnerimage/applink": "The app link: the MCP server through which a persistent agent reaches the application that runs it, and",
    "runnerimage/desktoplink": "The desktop link: the MCP servers through which a run reaches the applications installed on the person's own computer, and the relay that carries each call out of the sandbox to Agentry Desktop",
    "runnerimage/beneath": "A name beneath a directory: every open, make, remove, rename and ownership change resolves its path from a",
    "runnerimage/bridge": "The runner image's bridge role: a stdio MCP server the gateway launches like any declared server, which",
    "runnerimage/confine": "A declared exec command to the places it may write",
    "runnerimage/decode": "The runner image's `decode` role: it turns the documents under one directory into plain text under",
    "runnerimage/docpages": "The runner image's `pages` role: a document the Report Designer is given, a PDF's first pages rendered by poppler or a Word document's theme, running text and pictures, and the facts the host and the tools server agree on",
    "runnerimage/hostlink": "The runner's host link: the MCP server through which an agent works a target host, a machine the person",
    "runnerimage/ical": "Reads an iCalendar (RFC 5545) feed, the ICS document a calendar service publishes at a subscription",
    "runnerimage/messenger": "The messenger role of the runner image: the one container that holds a messenger account's credential and",
    "runnerimage/minimize": "The runner image's `minimize` role: run once by `docker build`, it turns the shared runner image into a",
    "runnerimage/netaddr": "The runner's one rule for a public address: the ranges web_read, the input-source proxy and the browse",
    "runnerimage/netproxy": "The llm-proxy of the agent-container topology: the run network is created --internal, so the ONLY way",
    "runnerimage/portrelay": "Carries one TCP connection to a port of the container's own loopback over stdio: the runner execs it in",
    "runnerimage/probe": "The runner image's gateway probe: the health check every run makes before its agent starts (an",
    "runnerimage/send": "The runner image's `deliver` role: the delivery step that hands a finished run's deliverables to a",
    "runnerimage/sockfilter": "The per-run filter between a run's MCP gateway and the docker daemon",
    "runnerimage/source": "The runner image's `source` role: the fetch step that brings a run's input source in before the run, as",
    "runnerimage/toolargs": "Decodes the arguments of a model's tool call",
    "runnerimage/toolsrv": "The Agentry companion tools server: one MCP stdio server whose tool surface is constructed from the",
    "runnerimage/webscan": "AI Review of what the Drafter's web tools bring back, before its model reads any of it",
    "scripts/context-measure": "Reading run and segment receipts to report how a conversation's context grew",
    "scripts/mcp-server-survey": "Installing and starting declared MCP servers the way the runner does, and recording what each did",
    "scripts/notices": "Licence classification and the generated third-party notices",
}

# The large packages hold several subjects in one binary; a test file joins
# the first subject whose pattern matches its name.
SUBJECTS = {
    "cmd/agentry": {
        "Persistent agent commands": r"cmd_agent",
        "Runhost and schedule commands": r"cmd_runhost|cmd_schedule|cmd_run_runhost",
        "Run commands and run output": r"cmd_run|run_summary|run_usage|rungrant|cmd_runs|cmd_runnertools|cmd_acp|attack_acp|output|nextsteps",
        "Publish, install and verification commands": r"cmd_publish|publish_stream|cmd_install|cmd_verify|cmd_yank|cmd_integrity|cmd_validate|cmd_fork|cmd_info|cmd_builtins|builtin_budgets|advisories|minversion|rulescache|optional_secrets|guide",
        "Draft, knowledge, voice and report commands": r"cmd_draft|cmd_knowledge|cmd_voice|cmd_report_design|report_theme|cmd_review|cmd_init",
        "Sign-in, account, model and setup commands": r"cmd_login|cmd_whoami|cmd_claude|cmd_chatgpt|cmd_copilot|cmd_modelsignins|envtoken|config|cmd_setup|cmd_doctor|askhidden",
        "Connections, destinations and capabilities": r"cmd_connect|cmd_destinations|cmd_deliver|cmd_browser|caps|cmd_profiles|cmd_files",
        "Other command surface": r".*",
    },
    "pkg/runner/engine": {
        "Persistent agent runs": r"persistent|messenger|calendar_secrets|live_events|applink",
        "Sign-ins, credits and model access": r"signin|credits|outofcredits|budget|usage|modelhosts|copilotmodels|keyring",
        "Inputs, sources and deliverables": r"inputs|inputreview|sources|sourcereview|deliverables|destinations|fetchsave|knowledge|voice|pdf|render|drivepath|stage|share|outbox",
        "Container topology, hosts and teardown": r"docker|filtersidecar|gateway|hardening|host|alive|teardown|sweep|recovery|owner|placement|podman|portrelay|relays|mirror|imagelabel|runlock|capacity|workdirs|heldcfg|heldopen|toolfilter|web|browser|remote",
        "Run execution, protocol and results": r".*",
    },
}

HINTS = {
    "docker interface (real or fake)": r"dockerx\.",
    "real docker daemon": r"exec\.Command\(\"docker\"|no docker",
    "HTTP transport": r"httptest\.|http\.Client",
    "child process": r"exec\.Command|os\.Executable",
    "source/file contract": r"os\.ReadFile|os\.Open|filepath\.",
    "real-time wait": r"time\.Sleep|time\.After|time\.NewTimer",
    "explicit skip": r"t\.Skip",
}


def inventory():
    paths = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=ROOT,
    ).decode().split("\0")
    groups = {}
    checks = []
    for name in sorted(set(p for p in paths if p.endswith("_test.go"))):
        path = ROOT / name
        if not path.exists():  # tracked deletions waiting to be staged
            continue
        package = str(path.parent.relative_to(ROOT))
        if package not in PURPOSES:
            raise ValueError(f"add a behavior owner for {package}")
        text = path.read_text()
        subject = PURPOSES[package]
        for label, pattern in SUBJECTS.get(package, {}).items():
            if re.search(pattern, path.name):
                subject = label
                break
        family = package + ": " + subject
        group = groups.setdefault(family, {
            "owner": package, "behavior": subject,
            "disposition": "retain", "files": [],
        })
        declarations = re.findall(r"^func ((?:Test|Fuzz|Benchmark)\w+)\(", text, re.M)
        # Direct source clues are discovery aids, not a complete dependency map.
        hints = [label for label, pattern in HINTS.items() if re.search(pattern, text)]
        group["files"].append({
            "path": name,
            "tests": [d for d in declarations if d.startswith("Test") and d != "TestMain"],
            "fuzz": [d for d in declarations if d.startswith("Fuzz")],
            "benchmarks": [d for d in declarations if d.startswith("Benchmark")],
            "fixtureEntryPoint": "TestMain" in declarations,
            "dependencyHints": hints,
        })
    for name in sorted(set(paths)):
        path = ROOT / name
        if not path.is_file():
            continue
        acceptance = name.startswith(("scripts/acceptance/", "scripts/windows-tests/")) and path.suffix in (".sh", ".py", ".ps1")
        image_check = re.match(r"scripts/verify-[\w-]+-image\.sh$", name) is not None
        selftest = name.startswith("scripts/") and ("selftest" in path.name or "self-test" in path.name)
        if acceptance or image_check or selftest:
            checks.append({"path": name, "disposition": "retain", "kind": "acceptance, image or self-test check"})
        elif name.startswith(".github/workflows/"):
            text = path.read_text()
            jobs = text.split("\njobs:\n", 1)
            checks.append({"path": name, "disposition": "retain", "kind": "workflow", "jobs": re.findall(r"^  ([a-zA-Z0-9_-]+):", jobs[1], re.M) if len(jobs) == 2 else []})
    return {
        "formatVersion": 1,
        "scope": "Retained named evidence; assertion equivalence is decided in each behavior batch, not inferred from this catalogue.",
        "families": [groups[k] for k in sorted(groups)],
        "acceptanceAndWorkflowEvidence": checks,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--write", action="store_true")
    mode.add_argument("--check", action="store_true")
    args = parser.parse_args()
    try:
        result = inventory()
        expected = json.dumps(result, indent=2, ensure_ascii=False) + "\n"
        if args.write:
            OUTPUT.parent.mkdir(parents=True, exist_ok=True)
            OUTPUT.write_text(expected)
        elif not OUTPUT.exists() or OUTPUT.read_text() != expected:
            actual = OUTPUT.read_text() if OUTPUT.exists() else ""
            sys.stdout.writelines(difflib.unified_diff(actual.splitlines(True), expected.splitlines(True), fromfile=str(OUTPUT.relative_to(ROOT)), tofile="current test evidence"))
            print("Test evidence inventory changed: run python3 scripts/testing/inventory.py --write", file=sys.stderr)
            return 1
        files = [f for g in result["families"] for f in g["files"]]
        print(f'{len(result["families"])} families, {len(files)} test files, {sum(len(f["tests"]) for f in files)} named tests, {sum(len(f["fuzz"]) for f in files)} fuzz targets, {len(result["acceptanceAndWorkflowEvidence"])} acceptance/workflow entries')
        return 0
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

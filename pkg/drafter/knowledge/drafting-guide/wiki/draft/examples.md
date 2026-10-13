---
title: Worked drafts
type: example
summary: Four whole drafts, each from a description to the manifest, the shape of its instructions and its notes; one with no tools, one with fetch and a key, one with a declared MCP server, each modelled on one of Agentry's first-party example QuickAgents, and one with a target host.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/agentry/target-hosts/, doc:servers.yaml]
updated: 2026-10-12
---

# Worked drafts

The first three drafts below are modelled on Agentry's first-party example QuickAgents, and every one passes the registry's rules as it stands. Copy the shape, not the content: a draft is written for the description in front of you [src: web:https://docs.secureagentry.net/quickagents/].

## No tools: meeting notes to action items

The description: "Turn our meeting notes into a list of decisions and who has to do what by when."

The task is text in, text out, so the draft declares nothing at all. The notes arrive as an input file, named in `guide.files`. The meeting date is a default in the task field, not a question: a run without one reads dates relative to the notes.

```json
{
  "schemaVersion": "2",
  "name": "meeting-action-items",
  "namespace": "draft",
  "version": "0.1.0",
  "summary": "Turns meeting notes or a transcript into the decisions made and the action items with owners and due dates.",
  "license": "Apache-2.0",
  "tags": ["meetings", "notes", "action-items"],
  "modes": ["autonomous"],
  "entrypoints": {"instruction": "AGENT.md"},
  "guide": {
    "overview": "Reads your meeting notes or transcript and returns report.md: the decisions the meeting made, each action item with its owner, due date and the line it came from, and the questions left open. It returns empty lists rather than invent items.",
    "prepare": ["The notes or transcript as a file.", "The meeting date, so that 'by Friday' becomes a date."],
    "files": "Add the notes or transcript: a .txt, .md, .docx or .vtt file.",
    "task": "e.g. Meeting on 2026-09-01 with Ana, Ben and Chris."
  },
  "capabilities": {
    "network": {"egress": "none"},
    "filesystem": {"read": "none", "write": "none"},
    "exec": "none",
    "dataAccess": ["files"]
  },
  "publish": {"include": ["agent.yaml", "AGENT.md", "README.md", "LICENSE"]}
}
```

Its instructions: `## Role` (extract decisions and commitments, never summarise the discussion); `## Inputs` (the notes file; the date and the attendees from the task, or from the notes when it gives none); `## Steps` (read every input, find the decisions, then the actions, then the open questions, each with the quoted line behind it); `## Output` (a skeleton of `report.md` with its three sections); `## Background` (what counts as a decision and as a commitment, how to resolve relative dates, how to treat "we should" against "I will"); `## Checks` (no owner invented, every item quoted, empty sections said to be empty).

Its notes: the date default, and that it writes the result as Markdown.

## fetch and a key: triaging GitHub issues

The description: "Look at the new issues in my GitHub repository and suggest labels and a priority for each."

GitHub's API is one host the run calls with its own `fetch`, so no server is needed. The API needs a token for a private repository, so the draft declares one key, with help in `guide.connect` asking for read-only access. The repository changes from run to run, so it is the one intake question.

```json
{
  "schemaVersion": "2",
  "name": "github-issue-triage",
  "namespace": "draft",
  "version": "0.1.0",
  "summary": "Proposes labels, a priority and likely duplicates for the newest unlabelled issues of a GitHub repository.",
  "license": "Apache-2.0",
  "tags": ["github", "issues", "triage"],
  "modes": ["autonomous"],
  "entrypoints": {"instruction": "AGENT.md"},
  "intake": [
    {"id": "repository", "prompt": "Which repository, as owner/name?", "type": "text", "required": true}
  ],
  "guide": {
    "overview": "Reads the newest unlabelled issues of one GitHub repository and proposes for each a priority from P0 to P3, labels from the repository's own list, and any likely duplicate. It returns report.md and changes nothing on GitHub.",
    "prepare": ["A GitHub token that can read the repository's issues."],
    "task": "e.g. The 10 newest unlabelled issues.",
    "connect": {
      "GITHUB_TOKEN": "A fine-grained personal access token that can only **read**:\n\n1. On GitHub, open Settings, Developer settings, Fine-grained tokens, and generate a new token.\n2. Under Repository access, choose the repository.\n3. Under Repository permissions, set **Issues** to *Read-only*."
    }
  },
  "capabilities": {
    "network": {"egress": ["api.github.com"]},
    "filesystem": {"read": "none", "write": "none"},
    "exec": "none",
    "secrets": ["GITHUB_TOKEN"],
    "dataAccess": ["tickets"]
  },
  "publish": {"include": ["agent.yaml", "AGENT.md", "README.md", "LICENSE"]}
}
```

Its instructions name each request in `## Steps`: `GET https://api.github.com/repos/{owner}/{repo}/labels`, then the open issues without a label, newest first, each with `fetch` and `auth` `{"env": "GITHUB_TOKEN"}`. `## Background` holds the priority scale and the signs of a duplicate. `## Checks` says what to do when the token cannot read the repository (a 404 for a private one).

Its notes: the default of the 10 newest issues, and that the token needs only read access.

## A declared MCP server: auditing dependencies

The description: "Check the packages in my lockfiles for known vulnerabilities and tell me what to upgrade."

The advisories come from OSV. A published server on *Runs here* asks OSV for many packages at once, with no key, so the draft pins it at an exact version and lists only the tools the steps call. Its host goes in the server's own `egress`, and the QuickAgent itself declares no network.

```json
{
  "schemaVersion": "2",
  "name": "dependency-advisory-audit",
  "namespace": "draft",
  "version": "0.1.0",
  "summary": "Checks the packages pinned in your lockfiles against OSV and reports the vulnerable ones with fixed versions.",
  "license": "Apache-2.0",
  "tags": ["dependencies", "vulnerabilities", "osv"],
  "modes": ["autonomous"],
  "entrypoints": {"instruction": "AGENT.md"},
  "guide": {
    "overview": "Checks every pinned package in the lockfiles you add against the OSV advisory database. Returns report.md, worst first, with the smallest fixed version of each.",
    "prepare": ["The project's lockfiles."],
    "files": "Add the lockfiles: package-lock.json, go.sum, uv.lock, Cargo.lock.",
    "task": "e.g. Audit every lockfile; put anything critical first."
  },
  "capabilities": {
    "network": {"egress": "none"},
    "filesystem": {"read": "none", "write": "none"},
    "exec": "none",
    "dataAccess": ["code"],
    "mcpServers": [
      {"npm": "@cyanheads/osv-advisory-mcp-server", "version": "0.1.15",
       "tools": ["osv_query_batch", "osv_get_vulnerability"], "egress": ["api.osv.dev"]}
    ]
  },
  "publish": {"include": ["agent.yaml", "AGENT.md", "README.md", "LICENSE"]}
}
```

It asks nothing before a run: the lockfiles are the input, and the severity to report is a default (all of them, worst first). The server works on the package names and versions the run passes in its calls, never on a file, which is why it fits: it never sees the lockfiles themselves [src: doc:servers.yaml].

Its notes: the severity default, the server and what the registry's check said of its version, and the closest published QuickAgent, if `registry_search` found one.

## A target host: setting up and hardening a server

The description: "Help me set up and harden a fresh Ubuntu server: updates, a firewall, SSH keys only, automatic security updates."

The server changes from run to run, so it is a target host: a `host` question, listed in `capabilities.targetHosts`. The run works it through the host tools, so the draft declares no command, no egress and no key: the server downloads its own packages, and the person's sign-in is asked for by the run setup, beside the address. The changes touch SSH and the firewall, so the steps are written never to lock the person out [src: web:https://docs.secureagentry.net/agentry/target-hosts/].

```json
{
  "schemaVersion": "2",
  "name": "ubuntu-server-hardening",
  "namespace": "draft",
  "version": "0.1.0",
  "summary": "Sets up and hardens an Ubuntu server you name: updates, a firewall, key-only SSH and automatic security updates.",
  "license": "Apache-2.0",
  "tags": ["linux", "ubuntu", "server", "hardening"],
  "modes": ["autonomous"],
  "entrypoints": {"instruction": "AGENT.md"},
  "intake": [
    {"id": "server", "prompt": "The Ubuntu server to harden (user@host, or user@host:port)", "type": "host", "required": true}
  ],
  "guide": {
    "overview": "Signs in to the Ubuntu server you name as the account you choose, checks its state, then applies updates, a firewall that keeps SSH open, key-only SSH once a key sign-in works, and automatic security updates. Returns report.md: every change, how to undo it, and what it left alone.",
    "prepare": ["An account on the server that can use sudo, and its sudo password if sudo asks for one.", "A key sign-in to that account, if SSH is to accept keys only."],
    "task": "e.g. Also allow ports 80 and 443 in the firewall."
  },
  "capabilities": {
    "network": {"egress": "none"},
    "filesystem": {"read": "none", "write": "none"},
    "exec": "none",
    "targetHosts": ["server"]
  },
  "publish": {"include": ["agent.yaml", "AGENT.md", "README.md", "LICENSE"]}
}
```

Its instructions: `## Role` (harden one Ubuntu server, changing only what the steps name); `## Inputs` (the server, and the task's extra ports or exceptions); `## Steps` (with `host_run`: read `/etc/os-release` and stop on a release it does not support; list what is listening and the current SSH and firewall settings; with `sudo: true`, update the packages non-interactively; allow SSH's port, then the task's ports, then enable the firewall; turn off password sign-in only when the run itself signed in with a key, after `sshd -t` passes, keeping a copy of the old file; enable unattended security upgrades; check each change after making it); `## Output` (a skeleton of `report.md`: the server's state before, each change with its undo, what it left alone and why); `## Background` (what each setting does, the Ubuntu releases it supports, what to do on one it does not); `## Checks` (SSH still reachable after every change, nothing removed it did not install, no credential printed).

Its notes: that it runs on the person's computer only, with the CLI or the desktop app, never In Agentry; that the account must be able to use sudo; and what it leaves alone by default.

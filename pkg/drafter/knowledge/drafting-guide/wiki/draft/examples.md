---
title: Worked drafts
type: example
summary: Four whole drafts, each from a description to the manifest, the shape of its instructions and its notes; one with no tools, one with fetch and a key, one with a declared MCP server, each modelled on one of Agentry's first-party example QuickAgents, and one that checks a server the person names (a target host).
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

## A target host: checking a server's health

The description: "Check the health of a Linux server I name: disk space, memory, failed services and recent errors in the logs, and tell me what needs attention."

The server changes from run to run, so it is a target host: a `host` question, listed in `capabilities.targetHosts`. The run works it through the host tools, so the draft declares no command, no egress and no key; the person's sign-in is asked for by the run setup, beside the address. The task only reads, so the run is autonomous and its steps change nothing [src: web:https://docs.secureagentry.net/agentry/target-hosts/].

```json
{
  "schemaVersion": "2",
  "name": "linux-server-health-check",
  "namespace": "draft",
  "version": "0.1.0",
  "summary": "Checks a Linux server you name for low disk space, memory pressure, failed services and recent log errors, and changes nothing.",
  "license": "Apache-2.0",
  "tags": ["linux", "server", "monitoring"],
  "modes": ["autonomous"],
  "entrypoints": {"instruction": "AGENT.md"},
  "intake": [
    {"id": "server", "prompt": "The server to check (user@host, or user@host:port)", "type": "host", "required": true}
  ],
  "guide": {
    "overview": "Signs in to the Linux server you name as the account you choose and reads its state: disk and inode use, memory and swap, failed systemd units, and the errors its logs recorded in the last day. Returns report.md with what needs attention first. It changes nothing on the server.",
    "prepare": ["An account on the server; one in the adm or systemd-journal group, or a sudo password, lets it read every log."],
    "task": "e.g. Look at the last week of logs instead of the last day."
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

Its instructions: `## Role` (report a server's health, change nothing); `## Inputs` (the server, and the task's period or focus); `## Steps` (with `host_run`: read `/etc/os-release` and the uptime and load; `df -h` and `df -i`; `free -m`; `systemctl --failed --no-pager`; `journalctl -p err --since "-1 day" --no-pager`, with `sudo: true` when the account cannot read the journal, and, when that is refused because the person gave no sudo password, a note of what it could not read; a long log is read in pages, never whole); `## Output` (a skeleton of `report.md`: what needs attention, worst first, each with the command output that shows it and what to do about it, then what looked normal); `## Background` (the thresholds it uses, such as a filesystem above 90% or swap in steady use, and what a failed unit or a repeated error usually means); `## Checks` (no command that changes the server, every finding backed by output, a credential the logs print reported by kind and place only).

Its notes: that it runs on the person's computer only, with the CLI or the desktop app, never In Agentry; which account it needs to read every log; and the thresholds it picked.

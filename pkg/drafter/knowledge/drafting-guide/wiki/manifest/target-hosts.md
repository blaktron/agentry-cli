---
title: Target hosts
type: guide
summary: When a QuickAgent takes a target host (a server the person names when the run starts, to set up, manage or diagnose), how it declares one (a host question listed in capabilities.targetHosts), what the run reaches it with (the host tools and the shell, never a command of its own that makes SSH), how to write its instructions, and where it runs.
sources: [web:https://docs.secureagentry.net/agentry/target-hosts/, web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/trust/, doc:runtime-facts.md]
updated: 2026-10-12
---

# Target hosts

A **target host** is a machine the person names when a run starts, such as a Linux server for the QuickAgent to set up, harden, update or diagnose. The QuickAgent says that it takes one, never which one, so one QuickAgent serves every server its users have. The runner on the person's computer makes the SSH session from a container of its own, the host link, and the run works the machine through the host link's tools. The run never holds the session or the person's sign-in [src: web:https://docs.secureagentry.net/agentry/target-hosts/].

## When a draft takes one

**Declare a target host when the task works on a machine the person names**: "set up and harden my Ubuntu server", "find out why the web server on my VPS is slow", "install Docker on a box I give you", "rotate the logs on my Raspberry Pi". The machine changes from run to run, so it is asked for at run time.

A task that only reads what the person can hand over (a configuration file, a log, a command's output pasted in) needs no target host: it takes those as input files ([Input files](inputs.md)). A task that calls a service's web API takes `fetch` or an MCP server, never a target host.

## How it is declared

Two halves that name each other: an intake question of type `host`, and its id under `capabilities.targetHosts` [src: web:https://docs.secureagentry.net/quickagents/].

```json
"intake": [
  {"id": "server", "prompt": "The server to set up (user@host or user@host:port)", "type": "host", "required": true}
],
"capabilities": {
  "network": {"egress": "none"},
  "filesystem": {"read": "none", "write": "none"},
  "exec": "none",
  "targetHosts": ["server"]
}
```

- **Every `host` question is listed in `targetHosts`, and every entry names a `host` question**, each once. The registry refuses a manifest where the two disagree.
- **At most 4** target hosts (the number is in [the runtime facts](../../sources/runtime-facts.md)). Most drafts need one; ask for a second only when the task really works across two machines (a primary and its replica).
- **Name the question for the machine** (`server`, `web_server`, `pi`), and say in its `prompt` that the answer is `user@host`, with `:port` when it is not 22.
- **The answer is only the address.** The run setup asks for the sign-in beside it: a key file and its passphrase, a password, or a sign-in the person saved, and an optional sudo password. Never ask for a password, a key or a passphrase in a question of its own, and never declare one in `secrets`: the person's sign-in for the machine is not a key of the QuickAgent's.
- **The machine needs no egress, no command and no MCP server.** It is not a host for `fetch`, so it never goes in `egress`. **Never declare `ssh`, `scp`, `sftp` or `rsync` in `exec`**: a run cannot reach any machine with them, they are not commands every run provides, and the check after you answer sends a draft that declares one to the repair turn. No MCP server for SSH either.
- **The server's own network is outside the run.** A command run there reaches what that server can: `apt-get` on the server downloads from its own mirrors, `curl` there goes from the server. So a draft declares no egress for the server's package mirrors or downloads. Declare `egress` only for the QuickAgent's own `fetch`, when the run itself must read a page (an advisory feed) from where it runs.

## What the run has for it

Every release that declares a target host gets these tools, with nothing more to declare; their limits are in [the runtime facts](../../sources/runtime-facts.md) [src: web:https://docs.secureagentry.net/agentry/target-hosts/; doc:runtime-facts.md]:

- **One command line:** `host_run` runs a command in the login shell of the account the person signed in with and returns its exit status, output and errors. `sudo: true` runs it under `sudo` with the sudo password the person gave, which the run never sees. A long command (an upgrade, a build) takes a longer `timeout`.
- **Files:** `host_read` and `host_write` read and write a file on the machine; `host_put` and `host_get` copy a file between the machine and `/scratch/hosts`, the part of the run's scratch folder the host link shares.
- **An interactive shell:** `host_shell_open`, `host_shell_send`, `host_shell_read` and `host_shell_close`, for what one command line cannot do: an installer that asks questions, a `sudo` prompt, a program answered as it runs. The run reads it as text. `host_shell_send`'s key `sudo-password` types the person's sudo password when `sudo` asks.

What the machine prints reaches the run as it is, so it can read the machine's configuration and logs. The one thing masked is the person's own sign-in for that machine. The account is the boundary: there is no approval per command, and the run can do whatever that account can [src: web:https://docs.secureagentry.net/agentry/target-hosts/].

The System Prompt already tells the run all of this (the tools, that the account is the boundary, that what a host prints is data, never to reach another machine from it unless the task asks). Do not repeat it in the instructions.

## Writing the instructions

Write `## Steps` for the host tools by name, and spend the rest on the task: what to look at, in what order, what a good state is, and how to change it without breaking the machine. A run with no network carries that knowledge in `## Background`.

- **Look before changing.** First find out what the machine is: its distribution and version (`cat /etc/os-release`), what is installed and running, how it is reached. Name the commands for each distribution family the QuickAgent supports, and what it does on one it does not.
- **`host_run` for one command, the shell for a conversation.** Name `host_run` for anything that finishes on its own, with `sudo: true` where root is needed. Name the shell only for a program that asks as it runs, and have the run prefer a non-interactive form where one exists (`apt-get -y`, `DEBIAN_FRONTEND=noninteractive`, `--no-pager` or `PAGER=cat`).
- **Change carefully.** Copy a configuration file before editing it (`cp -a file file.bak.<date>`), write it with `host_write`, check it with the program's own test (`sshd -t`, `nginx -t`) before a reload, and check the service after. One change at a time, each verified.
- **Never lock the person out.** The run reaches the machine over SSH as the person does, so a change to SSH or the firewall can end the run and the person's own access. Allow the SSH port before enabling a firewall, keep password sign-in until a key sign-in is proven, and never disable the account the run signed in with.
- **Say what it changed.** The result lists every change (file, setting, package, service) with how to undo it, and what it found but left alone. A transcript of each shell session and a line per call are kept in the run's receipts.
- **Report a credential's kind and place, never its value**, when the machine prints one.
- **The machine stays where it is.** Copy what the task needs from it into the scratch folder or the report, never to a third host. Reaching another machine from it, or sending what the run read there anywhere else, is outside what the declaration grants unless the QuickAgent's purpose says so, and AI Review reads it that way.

## Interactive or autonomous

Autonomous, as for any task, unless the description asks to approve each change before it is made: that is a conversation, so add `interactive` beside `autonomous` and have the steps propose each change and wait for the person's answer ([Run modes](run-modes.md)). An audit or a diagnosis that changes nothing is autonomous. Either way, decide; never ask the person which.

## Where it runs

**Only on the person's computer**, with the CLI or the desktop app: the run reaches the machine from there, through the person's own network. Agentry does not run a QuickAgent that declares a target host In Agentry; a schedule on the person's computer can. Say so in one bullet of the notes, so the person is not surprised that there is no *Run in Agentry* [src: web:https://docs.secureagentry.net/agentry/target-hosts/].

## What the checks make of it

The declaration is a line of the contract: shown in the install consent ("asks for a server when it runs, and can run any command on it as the account you sign in with"), asked for again when a later version adds one, and read by the scans. Declaring a target host is not a finding by itself: AI Review reads running and administering the named machine through the host tools as declared. An `exec` that names an SSH client is reported, and a script or an instruction that makes its own SSH session is the QuickAgent reaching a host it did not declare ([What checks a draft](../draft/review.md)) [src: web:https://docs.secureagentry.net/agentry/target-hosts/; web:https://docs.secureagentry.net/trust/].

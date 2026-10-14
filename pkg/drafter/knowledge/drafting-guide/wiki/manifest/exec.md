---
title: Commands
type: guide
summary: When a QuickAgent declares commands at all (computation the tools cannot do, never reading its inputs), the base commands every run provides, where a command can write, what a run In Agentry provides, and Unsafe Mode on the desktop.
sources: [doc:runtime-facts.md, web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/agentry/unsafe-mode/]
updated: 2026-10-12
---

# Commands

`exec` is `none` unless the task needs a command. An agent reads a CSV, a log, a document, a codebase or a diff itself, through its input tools, in the file's text or as uploaded, so most QuickAgents declare no command. With `exec: none` the run has no `run_command` tool at all [src: web:https://docs.secureagentry.net/quickagents/].

**Never `ssh`, `scp`, `sftp` or `rsync`.** A run reaches no machine with them, and none is a command every run provides. A QuickAgent that works on a server the person names declares a target host, and the run works it through the host tools ([Target hosts](target-hosts.md)).

**Never a browser of its own** (Chromium, Chrome, Firefox, Playwright, Puppeteer, Selenium, a WebDriver), as a command or as an MCP server. A QuickAgent that tests a website declares a browser, which the runner gives the run, and the run works it through the browser tools ([A browser](browser.md)). The check after you answer sends either to a repair turn.

## The base commands

Use `{commands: [...]}`, single command names, only for what reading cannot do, and only these, which every run provides (a run refuses any other): `base64`, `basename`, `bunzip2`, `bzcat`, `cat`, `cksum`, `cmp`, `comm`, `cut`, `diff`, `dirname`, `du`, `fold`, `grep`, `gunzip`, `head`, `hexdump`, `ls`, `md5sum`, `nl`, `od`, `paste`, `realpath`, `rev`, `sha1sum`, `sha256sum`, `sha512sum`, `sort`, `stat`, `strings`, `tac`, `tail`, `tar`, `tr`, `tree`, `uniq`, `unxz`, `unzip`, `wc`, `xzcat`, `zcat`. No shell, `awk`, `sed`, `find`, `curl` or `git` [src: doc:runtime-facts.md].

A run calls a declared command by its name, with arguments, and nothing else: `run_command` takes only the declared names, and a command's `PATH` holds only them. There is no pipe and no shell, so each step is one command [src: web:https://docs.secureagentry.net/runner/].

## Not for reading inputs

Commands are for computation the tools cannot do, never for reading the person's files. The runner unpacks every archive before the run, and `list_inputs`, `read_input` and `search_inputs` read every file, in its text or as uploaded (see [Input files](inputs.md)). So a draft never declares `cat`, `grep`, `ls`, `head`, `tail` or `tree` to read, search or list its inputs, and never declares `tar`, `unzip`, `gunzip`, `bunzip2`, `unxz`, `zcat`, `bzcat` or `xzcat` at all: the check after you answer sends an archive reader to a repair turn. What a command is for: a checksum (`sha256sum`), comparing two files (`diff`, `cmp`), a hex dump (`hexdump`, `od`), the printable strings of a binary (`strings`), counting or sorting a large file's lines (`wc`, `sort`, `uniq`).

## Where a command can write

Only beneath `/tmp`, which is discarded when the run ends, and the person's workspace when `filesystem.write` grants it. Never the output folder: reports go out through `write_report`, which needs no command [src: web:https://docs.secureagentry.net/runner/].

## Where it runs

- **On the person's computer.** The base commands need nothing from the person. Any other command needs an entry in that computer's command catalogue, which a new installation does not have, so a draft that declares one is refused there before it starts [src: web:https://docs.secureagentry.net/runner/].
- **In Agentry.** A hosted run provides the base commands and no others. A QuickAgent that declares any other command is refused when the run is created.
- **In Unsafe Mode,** a switch in the desktop app. Declared commands outside the base set are installed for the run from Kali Linux's repository, and a command can start any program its image holds. Only a run on that person's computer, with the switch on, gets it [src: web:https://docs.secureagentry.net/agentry/unsafe-mode/].

A draft declares only base commands: the check after you answer sends any other command to a repair turn. When part of the task cannot be done without another command (a scanner, an interpreter, a converter), do what the base commands and reading allow, and say in the notes which command the rest needs and that only the desktop's Unsafe Mode provides it.

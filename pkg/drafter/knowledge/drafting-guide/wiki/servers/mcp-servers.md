---
title: Declared MCP servers
type: guide
summary: When a server that needs a key or an account is the right one (only when nothing key-free does the task, and never a hosted browser), what a declared MCP server meets in the runner (a package installed, started and reached, or a vendor's remote server reached at its address), what that rules out, how a server signs in, how to look a server up in the registry's MCP server list, and how to check a server before you pin it. Read it before you declare any server.
sources: [doc:runtime-facts.md, doc:servers.yaml, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/quickagents/, web:https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization, web:https://docs.npmjs.com/cli/v11/commands/npm-install, web:https://docs.npmjs.com/cli/v11/configuring-npm/package-json, web:https://pip.pypa.io/en/stable/cli/pip_install/, web:https://peps.python.org/pep-0656/, web:https://packaging.python.org/en/latest/specifications/entry-points/, web:https://modelcontextprotocol.io/specification/2025-06-18/basic/transports]
updated: 2026-10-22
---

# Declared MCP servers

A declared MCP server is listed under `capabilities.mcpServers` because the QuickAgent's task needs the tools it provides. It is one of two kinds. A **package server** is an npm or PyPI package, pinned at an exact version: the runner installs it for that exact release and runs it in a container of its own, behind the run's gateway, with only its own hosts, keys and configuration. A **remote server** is a vendor's own MCP server, declared by its `https://` address: it runs at the vendor, and the runner reaches it through one relay to that address's host ([Remote servers](#remote-servers)) [src: web:https://docs.secureagentry.net/runner/; web:https://docs.secureagentry.net/quickagents/].

When a vendor has its own remote server and a package for the same service takes an API key instead, prefer the vendor's remote server, unless the task needs a tool only the package has. It is the vendor's own code, it reaches only the vendor's host, and it signs the person in on the vendor's own consent screen, so a run gets a short-lived access token rather than a long-lived key. [Remote servers and sign-ins](sign-ins.md) lists the vendors' servers and how each signs in.

Most servers published on npm and PyPI were written for a person's laptop. The runner is not one, and a server that works on a laptop can fail here. Some fail at install. Others install, start, list their tools and then fail on every call. Each point below is how the runner works today. Pin a server only when it passes all of them; otherwise use no server, or `fetch` to exact hosts, or ask for the data as an input file.

First look the server up on the two lists, [Runs here](runs-here.md) and [Does not run here](does-not-run-here.md). They hold what a survey of 50 servers saw in the runner on 2026-10-05: which ones started, listed their tools and answered, and why the others failed. Never pin a server on *Does not run here*, or one whose own dependencies include a package listed at that page's end: the check after you answer sends either to a repair turn, and the notes say what admitted each server you keep. A server on *Runs here* still needs what its entry lists (its keys, its hosts, an `env` setting) and tools that fit the task. For a server on neither list, look it up in the registry's MCP server list, then follow [Judging a server on neither list](#judging-a-server-on-neither-list) [src: doc:servers.yaml].

## A key or an account only when nothing else does the task

Before you declare a server, or `fetch` to an API, that needs a key, an account or a paid plan at its vendor, look for a way that needs none, in this order: the run's own tools ([How a run works](../runtime/how-a-run-works.md)); the browser the runner gives, for a website to test or read as a person sees it ([A browser](../manifest/browser.md)); the QuickAgent's own `fetch` to an API that answers without a key ([Egress and fetch](../manifest/egress-and-fetch.md)); a server that needs no key (its entry on [Runs here](runs-here.md) says so). A key means every person who runs the QuickAgent signs up at the vendor, and often pays, before the first run, and their data goes to one more company. A vendor's free tier still needs an account and a key, so it counts as one. Declare a keyed server or API only when none of those does the task, and say in the notes, in one bullet, which key-free ways you looked at and why each falls short. When a key adds something to a task a key-free way already does (a higher rate limit, private repositories beside public ones), declare it optional ([Keys](../manifest/secrets.md)).

## How it is installed

- **npm on Alpine Linux (musl), PyPI on Debian (glibc).** npm servers install and run on Node 26.13.0, PyPI servers on Python 3.15.2, one runtime per ecosystem (see [the numbers](#the-numbers)). A native binary built for glibc does not load in the npm runtime, which is musl [src: doc:runtime-facts.md].
- **npm: no install scripts.** The package is installed with `--ignore-scripts`. No `install`, `postinstall`, `prepare` or node-gyp step runs. A native addon that compiles at install, or a package that downloads a binary at install (a browser for puppeteer or playwright, a prisma engine, some image libraries), arrives without it and fails when it is used. A prebuilt binary shipped as an optional dependency installs only when the package publishes one for musl [src: web:https://docs.npmjs.com/cli/v11/commands/npm-install; doc:runtime-facts.md].
- **PyPI: wheels only, for the server and every dependency.** Nothing is built from source. Every package in the server's dependency tree needs a pure-Python wheel (`py3-none-any`) or a `manylinux` wheel for CPython 3.15 on x86_64 and aarch64. A dependency that publishes only a source distribution, or only `musllinux`, Windows or macOS wheels, can never install. `manylinux` wheels install because the PyPI runtime is glibc, so native packages such as `onnxruntime` and `scikit-learn` do; a `musllinux` wheel is not enough [src: web:https://pip.pypa.io/en/stable/cli/pip_install/; web:https://peps.python.org/pep-0600/; doc:runtime-facts.md].
- **Only from the package registry.** During install the builder reaches the registry and the file hosts its metadata names, nothing else. A dependency fetched from a git URL or any other host is blocked.
- **The version's own runtime range.** The version's `engines.node` (npm) or `requires_python` (PyPI) must admit the runtime's version. A version whose range excludes it is refused at install, or installs and fails every call. When the newest version excludes the runtime, pin the newest one whose range admits it [src: web:https://docs.npmjs.com/cli/v11/configuring-npm/package-json; doc:runtime-facts.md].

## How it is started

- **As exactly one program, with no arguments.** An npm server starts as its package's `bin` (the entry named after the unscoped package, else the first). A PyPI server starts as its console script (the one named after the project, else its only one, else the first). The runner passes no argument and no flag. A server that needs `stdio`, `--stdio`, `--transport stdio`, `serve`, a path or any other argument cannot be started. Only its `env` entries and its declared keys reach it [src: web:https://docs.npmjs.com/cli/v11/configuring-npm/package-json; web:https://packaging.python.org/en/latest/specifications/entry-points/].
- **An npm package with no `bin` cannot be declared**, and a PyPI package with no console script (one meant for `python -m`) does not start.
- **Over stdio only.** The runner talks to a package server on its standard input and output. A package that serves only HTTP or SSE cannot be declared as a package. When it is a vendor's own hosted server, declare it by its address instead ([Remote servers](#remote-servers)) [src: web:https://modelcontextprotocol.io/specification/2025-06-18/basic/transports].

## What it can reach while it runs

- **Its own files, read-only, and none of the run's.** The server runs as `root`, with HOME `/root`. Its installed files are read-only. Writes to HOME and to `/tmp` work but are discarded when the run ends, so a server that keeps a file (a memory, a cache, a log) must be pointed at `/tmp` through its `env`, and a server that writes into its own installed files fails. The survey's `runtimes` entries record this for both images [src: doc:servers.yaml]. The person's inputs, the workspace and the reports folder are not in its container at all. A server that works on local paths (a filesystem server, a git repository, a SQLite file, a folder of documents) sees nothing of the person's.
- **Only the hosts its `egress` names, on port 443.** Each declared host is reached over TCP port 443, with TLS end to end. Plain HTTP, a database port, SMTP and any other port fail. A redirect to a host not in the list fails, and so does a login, CDN or storage host the server reaches behind the scenes, so name every host it talks to. `"*"` is refused for a server. With no `egress`, the server has no network at all [src: doc:runtime-facts.md; web:https://docs.secureagentry.net/runner/].
- **No other programs.** The container holds the server's runtime and a shell (Node and npm with busybox and its small `wget`, or Python and pip with bash), nothing more: no git, no curl, no browser, no Docker. A server that runs another program to do its work fails when it calls it [src: doc:servers.yaml].
- **Keys as environment variables.** Each name in the server's `secrets` reaches it as an environment variable of that name, and the model never sees the value. A package server that opens a browser window to sign in, or keeps a credentials file of its own, cannot run. One that takes a token from an environment variable can, and so can a package whose sign-in [Remote servers and sign-ins](sign-ins.md#package-sign-ins) lists ([A package's sign-in](#a-packages-sign-in)) [src: web:https://docs.secureagentry.net/runner/].
- **Results that look like credentials are blocked.** The runner replaces a tool result that looks like it holds a key or a token with an error. A server whose results echo tokens or carry key-shaped strings fails on those calls.
- **Limits.** Each container gets the memory and CPU limits in [the numbers](#the-numbers), by default, and on Linux the process limit too [src: doc:runtime-facts.md].

## Pinning it

- **An exact version** you read from the package registry (`https://registry.npmjs.org/<package>/latest`, `https://pypi.org/pypi/<package>/json`), never a range and never `latest`. The pin is part of the release [src: web:https://docs.secureagentry.net/quickagents/].
- **A version that runs where the runs put it:** npm servers run on Node 26.13.0 and PyPI servers on Python 3.15.2. Read the version's `engines.node` (npm) or `info.requires_python` (PyPI) in the same metadata. When it excludes that Node or Python, take the newest version whose range admits it (`https://registry.npmjs.org/<package>` lists every version with its `engines`), or another server [src: doc:runtime-facts.md].
- **The registry's check of that version,** read with `registry_search` (`ecosystem`, `package`, `version`), below.
- **Only the tools the task calls,** in `tools`, by their exact names from the server's documentation or its entry on [Runs here](runs-here.md).
- **Its hosts, keys and settings on the server's own entry:** `egress` (exact hosts, never `"*"`), `secrets` (the names it reads its keys from) and `env` (plain settings, never a key). Prefer a server that needs no key, and one whose hosts you can name.

## Remote servers

A vendor's remote server is declared by its address, not a package:

```yaml
- url: https://mcp.linear.app/mcp
  auth: oauth          # oauth | secret | none
  scopes: [read]       # only with auth: oauth
  tools: [list_issues, get_issue]
```

- **Its address.** `https://`, on a public host name, port 443, with no user name, query or fragment, at most 512 characters. Its host is the only host it reaches, so it takes no `egress`, no `version` and no `env`: it runs at the vendor, at the address you declare [src: web:https://docs.secureagentry.net/quickagents/].
- **How it signs in** (`auth`). `oauth` signs the person in at the vendor; `secret` sends the server's one declared key as a bearer token (declare exactly one in its `secrets`); `none` sends nothing.
- **With `oauth`, consent at the first run.** The first run of this version asks whoever runs the QuickAgent to sign in at the vendor and allow the `scopes` you declare, and keeps that sign-in for this version alone; a new version asks again. Declare in `scopes` only the ones the task needs, from those the vendor offers ([Remote servers and sign-ins](sign-ins.md#remote-servers) lists them), and prefer a read-only scope or the read-only address a vendor offers (GitHub's ends `/readonly`). A vendor that offers a single scope (`default`) needs none named; the check after you answer sends an OAuth server with no scopes, where its vendor offers several, to a repair turn. A run gets a short-lived access token, never a refresh token [src: web:https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization; web:https://docs.secureagentry.net/quickagents/].
- **Whose client.** Some vendors let Agentry register its own client; others need the person to make an OAuth client at the vendor once, and the run setup shows how. [Remote servers and sign-ins](sign-ins.md#remote-servers) says which, and when you declare one that needs the person's own client, say so in the guide's help for running it.
- **What Agentry checks.** The address, how it signs in and the scopes, not the vendor's code: the registry's pipeline reports each remote server by its address and scopes when the version is submitted. A run In Agentry reaches only the vendors its deployment allows; another is refused with a sentence that names it.
- **Only the tools the task calls,** in `tools`, by the names the vendor's documentation gives.

## A package's sign-in

A package server may sign the person in through an `oauth` block: the runner makes the connection with the block's issuer and writes the access token into the files and settings the block names, which the package reads. A token file the package does not read fails silently: the run starts, and the server never gets the token. So give a package an `oauth` block only by copying, exactly, a sign-in [Remote servers and sign-ins](sign-ins.md#package-sign-ins) lists for that version, with the `egress` and `env` it lists. The check after you answer sends any other `oauth` block to a repair turn. A package with no listed sign-in that needs the person's account is not declared: use the vendor's remote server, or a package that takes a key. A package the registry knows cannot run on an access token alone, `mcp-gsuite` first, is refused a sign-in by name, with the one to declare instead (`workspace-mcp`) [src: doc:servers.yaml; web:https://docs.secureagentry.net/quickagents/].

## Before a run

At most eight servers, remote ones included. The registry looks every declared package version up in OSV (a remote server has no version to look up), and `registry_search` (with `ecosystem`, `package` and `version`) reads its answer. A version with an advisory the registry's gate rates high or critical is refused before a run starts, and a run In Agentry has no way past that. Prefer a version with no known advisories [src: doc:runtime-facts.md; web:https://docs.secureagentry.net/runner/].

## Checking a server before you pin it

Read the version's metadata from its registry (`https://registry.npmjs.org/<package>` lists every version with its `bin`, `engines`, `scripts` and `dependencies`; `https://pypi.org/pypi/<package>/<version>/json` has `requires_python`, `requires_dist` and the files with their wheel tags) and its README. Then check each point:

1. **It starts with no arguments.** The README's start command is the bare package (`npx <package>`, `uvx <package>`) with nothing after it, or the arguments it shows are optional.
2. **It speaks stdio.** Not "HTTP only", "SSE" or "streamable HTTP only". A vendor's own hosted server is declared by its address instead ([Remote servers](#remote-servers)).
3. **It has a program to start.** An npm `bin`, or a PyPI console script.
4. **Its runtime range admits Node 26.13.0 or Python 3.15.2.**
5. **It needs nothing built or fetched at install.** No `install` or `postinstall` script it depends on. No dependency on a native module such as `sharp`, `better-sqlite3`, `canvas`, `node-pty`, `keytar`, `puppeteer`, `playwright` or `onnxruntime`. For PyPI, no dependency that publishes only `manylinux` wheels or only a source distribution.
6. **It works on what it is given, not on files.** Its tools take their data in arguments or fetch it from an API. They do not read a local path.
7. **It runs no other program.** It does not call `git`, a browser, Docker or a command-line tool.
8. **Every host it talks to is an HTTPS host you can name.** List each in its `egress`. If its README cannot tell you the hosts, prefer another server.
9. **It authenticates with a key in an environment variable,** or needs none. List that variable in its `secrets`.

A server that fails any point does not run here. Choose another, or do the task without one. When you are unsure of a point, say so in the notes.

## Judging a server on neither list

**Look it up first.** `registry_search` with `mcp_server` (a package name such as `@scope/name`, a remote server's `https://` address, or a few words) reads the registry's MCP server list: a later survey of about 60 000 npm packages, PyPI packages and remote addresses from the MCP Registry, Smithery and GLAMA, run with the runner's own install, gateway and remote probe. Each line gives one status with its date. *works*: a package installed, started behind the gateway and listed its tools, or an address listed its tools with no credential. *sign-in*: an address publishes OAuth details the CLI accepts, and nobody signed in. *needs-key*: an address wants a key some other way. *fails*: it did not install, did not start or list a tool, or the address refused or did not answer; the line quotes what was seen. *not-declarable*: an address a manifest cannot declare. That survey declared each server from its catalogue's entry, with no hosts and no settings, so a failure may only mean a host or a setting it needed was missing: read what was seen. Prefer a server the list says works or signs in, at the version it names, and still check it against the points below, because listing tools is not doing the task. The list is evidence from its date, never a reason on its own to drop or keep a server, and a server it never reached may still run. A self-hosted registry may have no list.

The survey of 2026-10-05 found the same few failures again and again. Check for each one before you pin a server that is on neither list [src: doc:servers.yaml]:

- **A hosted browser is never the way.** A hosted browser service (Browserless, Browserbase, browser-use, Hyperbrowser, Steel) runs the browser at its vendor, needs an account and a key there, and opens the person's site from the vendor's machines. The runner gives a browser with neither ([A browser](../manifest/browser.md)), and the check after you answer sends a hosted one, by its package or its address, to a repair turn.
- **Listing tools is not doing the task.** Browser servers (Playwright, Puppeteer, Chrome DevTools) installed, started and listed every tool, then failed on the first call because there is no browser. A filesystem server, a PDF toolkit and a shell server listed their tools, but every tool takes a local path, and none of the person's files are in the server's container. Judge a server by what its tools need, not by whether it starts.
- **It fails at start without what it needs.** Several servers exit at start: one that wants a database URL as an argument, one that wants a subcommand (`server`, `server start`), one that reads its URL and token from the environment and has neither. An argument cannot be given here. A value in an environment variable can, through `env` or `secrets`, so read the README for which the server takes.
- **It reaches a host you did not declare.** Two servers failed until every host they talk to was declared: one also reads its documentation site when it lists its tools, and one fetches a service list from another host as it starts. List every host the README or the error names. When the README cannot tell you, prefer another server.
- **It is a client for something on the same machine.** Servers that talk to a desktop application (Blender), a local development server, a database or a cache (PostgreSQL, MySQL, MongoDB, Redis) start, but what they connect to is not there. A database's own port cannot be reached even when it is on the internet.
- **A native piece with no wheel for Linux.** A PyPI dependency that publishes no `manylinux` wheel for CPython 3.12 (only a source distribution) stops the install. An npm package that ships a prebuilt program for Linux, built for glibc, installs and then fails at start (`spawn … ENOENT`), because the npm runtime is musl. Check the wheels of the server's heavy dependencies on PyPI, and whether an npm server's README mentions a bundled binary.
- **Archived or abandoned, and unpinned.** Two archived reference servers fail at start on any machine today, because they depend on `mcp` with no upper bound and the SDK's API has moved on. Prefer a server with a release in the last few months, and a maintained one over an archived one.
- **One bad server stops the others.** One server listed a tool whose input schema the runner's gateway refuses, and the gateway stopped, taking every server of the run with it. This cannot be seen from the README. It is on *Does not run here* where it was seen.
- **The newest version may not fit.** For one server, 107 releases after the one that runs need a newer Python than the runtime's. Take the newest version whose range admits the runtime, as the check above says.

When one of these applies, do not pin the server. Say in the notes which one, and do the task another way, or without the server.

## The numbers

The values the runner uses by default, generated from its code, are in the source document [runtime facts](../../sources/runtime-facts.md): the two runtime images and their versions, the most servers a QuickAgent may declare, the port a server's hosts are reached on, the limits on each container, and the base commands every runner provides [src: doc:runtime-facts.md].

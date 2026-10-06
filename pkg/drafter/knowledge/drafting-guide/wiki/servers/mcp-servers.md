---
title: Declared MCP servers
type: guide
summary: What a declared MCP server meets in the runner (how it is installed, started and reached, and what it can reach), what that rules out, and how to check a server before you pin it. Read it before you pin any server.
sources: [doc:runtime-facts.md, doc:servers.yaml, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/quickagents/, web:https://docs.npmjs.com/cli/v11/commands/npm-install, web:https://docs.npmjs.com/cli/v11/configuring-npm/package-json, web:https://pip.pypa.io/en/stable/cli/pip_install/, web:https://peps.python.org/pep-0656/, web:https://packaging.python.org/en/latest/specifications/entry-points/, web:https://modelcontextprotocol.io/specification/2025-06-18/basic/transports]
updated: 2026-10-06
---

# Declared MCP servers

A declared MCP server is an npm or PyPI package, pinned at an exact version, that a QuickAgent lists under `capabilities.mcpServers` because its task needs the tools that server provides. The runner installs it for that exact release and runs it in a container of its own, behind the run's gateway, with only its own hosts, keys and configuration [src: web:https://docs.secureagentry.net/runner/; web:https://docs.secureagentry.net/quickagents/].

Most servers published on npm and PyPI were written for a person's laptop. The runner is not one, and a server that works on a laptop can fail here. Some fail at install. Others install, start, list their tools and then fail on every call. Each point below is how the runner works today. Pin a server only when it passes all of them; otherwise use no server, or `fetch` to exact hosts, or ask for the data as an input file.

First look the server up on the two lists, [Runs here](runs-here.md) and [Does not run here](does-not-run-here.md). They hold what a survey of 50 servers saw in the runner on 2026-10-05: which ones started, listed their tools and answered, and why the others failed. Never pin a server on *Does not run here*, or one whose own dependencies include a package listed at that page's end: the check after you answer sends either to a repair turn, and the notes say what admitted each server you keep. A server on *Runs here* still needs what its entry lists (its keys, its hosts, an `env` setting) and tools that fit the task. For a server on neither list, follow [Judging a server on neither list](#judging-a-server-on-neither-list) [src: doc:servers.yaml].

## How it is installed

- **On Alpine Linux, so musl, not glibc.** npm servers install and run on Node 24.21.0, PyPI servers on Python 3.12.14, one runtime per ecosystem (see [the numbers](#the-numbers)). A native binary built for glibc does not load on musl [src: doc:runtime-facts.md].
- **npm: no install scripts.** The package is installed with `--ignore-scripts`. No `install`, `postinstall`, `prepare` or node-gyp step runs. A native addon that compiles at install, or a package that downloads a binary at install (a browser for puppeteer or playwright, a prisma engine, some image libraries), arrives without it and fails when it is used. A prebuilt binary shipped as an optional dependency installs only when the package publishes one for musl [src: web:https://docs.npmjs.com/cli/v11/commands/npm-install; doc:runtime-facts.md].
- **PyPI: wheels only, for the server and every dependency.** Nothing is built from source. Every package in the server's dependency tree needs a pure-Python wheel (`py3-none-any`) or a `musllinux` wheel for CPython 3.12. A dependency that publishes only `manylinux` (glibc) wheels can never install. `onnxruntime` is one, so any server that pulls it in, however deep in its tree, cannot run here [src: web:https://pip.pypa.io/en/stable/cli/pip_install/; web:https://peps.python.org/pep-0656/; doc:runtime-facts.md].
- **Only from the package registry.** During install the builder reaches the registry and the file hosts its metadata names, nothing else. A dependency fetched from a git URL or any other host is blocked.
- **The version's own runtime range.** The version's `engines.node` (npm) or `requires_python` (PyPI) must admit the runtime's version. A version whose range excludes it is refused at install, or installs and fails every call. When the newest version excludes the runtime, pin the newest one whose range admits it [src: web:https://docs.npmjs.com/cli/v11/configuring-npm/package-json; doc:runtime-facts.md].

## How it is started

- **As exactly one program, with no arguments.** An npm server starts as its package's `bin` (the entry named after the unscoped package, else the first). A PyPI server starts as its console script (the one named after the project, else its only one, else the first). The runner passes no argument and no flag. A server that needs `stdio`, `--stdio`, `--transport stdio`, `serve`, a path or any other argument cannot be started. Only its `env` entries and its declared keys reach it [src: web:https://docs.npmjs.com/cli/v11/configuring-npm/package-json; web:https://packaging.python.org/en/latest/specifications/entry-points/].
- **An npm package with no `bin` cannot be declared**, and a PyPI package with no console script (one meant for `python -m`) does not start.
- **Over stdio only.** The runner talks to a server on its standard input and output. A server that serves only HTTP or SSE, or a remote server at a URL, cannot be declared [src: web:https://modelcontextprotocol.io/specification/2025-06-18/basic/transports].

## What it can reach while it runs

- **Its own files, read-only, and none of the run's.** The server runs as `root`, with HOME `/root`. Its installed files are read-only. Writes to HOME and to `/tmp` work but are discarded when the run ends, so a server that keeps a file (a memory, a cache, a log) must be pointed at `/tmp` through its `env`, and a server that writes into its own installed files fails. The survey's `runtimes` entries record this for both images [src: doc:servers.yaml]. The person's inputs, the workspace and the reports folder are not in its container at all. A server that works on local paths (a filesystem server, a git repository, a SQLite file, a folder of documents) sees nothing of the person's.
- **Only the hosts its `egress` names, on port 443.** Each declared host is reached over TCP port 443, with TLS end to end. Plain HTTP, a database port, SMTP and any other port fail. A redirect to a host not in the list fails, and so does a login, CDN or storage host the server reaches behind the scenes, so name every host it talks to. `"*"` is refused for a server. With no `egress`, the server has no network at all [src: doc:runtime-facts.md; web:https://docs.secureagentry.net/runner/].
- **No other programs.** The container holds busybox (with its small `wget`) and the server's runtime (Node and npm, or Python and pip), nothing more: no git, no curl, no browser, no Docker. A server that runs another program to do its work fails when it calls it [src: doc:servers.yaml].
- **Keys as environment variables.** Each name in the server's `secrets` reaches it as an environment variable of that name, and the model never sees the value. A server that signs in through a browser window (OAuth) cannot run. One that takes a token from an environment variable can [src: web:https://docs.secureagentry.net/runner/].
- **Results that look like credentials are blocked.** The runner replaces a tool result that looks like it holds a key or a token with an error. A server whose results echo tokens or carry key-shaped strings fails on those calls.
- **Limits.** Each container gets the memory and CPU limits in [the numbers](#the-numbers), by default, and on Linux the process limit too [src: doc:runtime-facts.md].

## Pinning it

- **An exact version** you read from the package registry (`https://registry.npmjs.org/<package>/latest`, `https://pypi.org/pypi/<package>/json`), never a range and never `latest`. The pin is part of the release [src: web:https://docs.secureagentry.net/quickagents/].
- **A version that runs where the runs put it:** npm servers run on Node 24.21.0 and PyPI servers on Python 3.12.14. Read the version's `engines.node` (npm) or `info.requires_python` (PyPI) in the same metadata. When it excludes that Node or Python, take the newest version whose range admits it (`https://registry.npmjs.org/<package>` lists every version with its `engines`), or another server [src: doc:runtime-facts.md].
- **The registry's check of that version,** read with `registry_search` (`ecosystem`, `package`, `version`), below.
- **Only the tools the task calls,** in `tools`, by their exact names from the server's documentation or its entry on [Runs here](runs-here.md).
- **Its hosts, keys and settings on the server's own entry:** `egress` (exact hosts, never `"*"`), `secrets` (the names it reads its keys from) and `env` (plain settings, never a key). Prefer a server that needs no key, and one whose hosts you can name.

## Before a run

At most eight servers. The registry looks every declared version up in OSV, and `registry_search` (with `ecosystem`, `package` and `version`) reads its answer. A version with an advisory the registry's gate rates high or critical is refused before a run starts, and a run In Agentry has no way past that. Prefer a version with no known advisories [src: doc:runtime-facts.md; web:https://docs.secureagentry.net/runner/].

## Checking a server before you pin it

Read the version's metadata from its registry (`https://registry.npmjs.org/<package>` lists every version with its `bin`, `engines`, `scripts` and `dependencies`; `https://pypi.org/pypi/<package>/<version>/json` has `requires_python`, `requires_dist` and the files with their wheel tags) and its README. Then check each point:

1. **It starts with no arguments.** The README's start command is the bare package (`npx <package>`, `uvx <package>`) with nothing after it, or the arguments it shows are optional.
2. **It speaks stdio.** Not "HTTP only", "SSE", "streamable HTTP only" or "remote server".
3. **It has a program to start.** An npm `bin`, or a PyPI console script.
4. **Its runtime range admits Node 24.21.0 or Python 3.12.14.**
5. **It needs nothing built or fetched at install.** No `install` or `postinstall` script it depends on. No dependency on a native module such as `sharp`, `better-sqlite3`, `canvas`, `node-pty`, `keytar`, `puppeteer`, `playwright` or `onnxruntime`. For PyPI, no dependency that publishes only `manylinux` wheels or only a source distribution.
6. **It works on what it is given, not on files.** Its tools take their data in arguments or fetch it from an API. They do not read a local path.
7. **It runs no other program.** It does not call `git`, a browser, Docker or a command-line tool.
8. **Every host it talks to is an HTTPS host you can name.** List each in its `egress`. If its README cannot tell you the hosts, prefer another server.
9. **It authenticates with a key in an environment variable,** or needs none. List that variable in its `secrets`.

A server that fails any point does not run here. Choose another, or do the task without one. When you are unsure of a point, say so in the notes.

## Judging a server on neither list

The survey of 2026-10-05 found the same few failures again and again. Check for each one before you pin a server that is on neither list [src: doc:servers.yaml]:

- **Listing tools is not doing the task.** Browser servers (Playwright, Puppeteer, Chrome DevTools) installed, started and listed every tool, then failed on the first call because there is no browser. A filesystem server, a PDF toolkit and a shell server listed their tools, but every tool takes a local path, and none of the person's files are in the server's container. Judge a server by what its tools need, not by whether it starts.
- **It fails at start without what it needs.** Several servers exit at start: one that wants a database URL as an argument, one that wants a subcommand (`server`, `server start`), one that reads its URL and token from the environment and has neither. An argument cannot be given here. A value in an environment variable can, through `env` or `secrets`, so read the README for which the server takes.
- **It reaches a host you did not declare.** Two servers failed until every host they talk to was declared: one also reads its documentation site when it lists its tools, and one fetches a service list from another host as it starts. List every host the README or the error names. When the README cannot tell you, prefer another server.
- **It is a client for something on the same machine.** Servers that talk to a desktop application (Blender), a local development server, a database or a cache (PostgreSQL, MySQL, MongoDB, Redis) start, but what they connect to is not there. A database's own port cannot be reached even when it is on the internet.
- **A native piece built for glibc.** A PyPI dependency with no `musllinux` wheel stops the install (`onnxruntime`, `scikit-learn`). An npm package that ships a prebuilt program for Linux, built for glibc, installs and then fails at start (`spawn … ENOENT`). Check the wheels of the server's heavy dependencies on PyPI, and whether an npm server's README mentions a bundled binary.
- **Archived or abandoned, and unpinned.** Two archived reference servers fail at start on any machine today, because they depend on `mcp` with no upper bound and the SDK's API has moved on. Prefer a server with a release in the last few months, and a maintained one over an archived one.
- **One bad server stops the others.** One server listed a tool whose input schema the runner's gateway refuses, and the gateway stopped, taking every server of the run with it. This cannot be seen from the README. It is on *Does not run here* where it was seen.
- **The newest version may not fit.** For one server, 107 releases after the one that runs need a newer Python than the runtime's. Take the newest version whose range admits the runtime, as the check above says.

When one of these applies, do not pin the server. Say in the notes which one, and do the task another way, or without the server.

## The numbers

The values the runner uses by default, generated from its code, are in the source document [runtime facts](../../sources/runtime-facts.md): the two runtime images and their versions, the most servers a QuickAgent may declare, the port a server's hosts are reached on, the limits on each container, and the base commands every runner provides [src: doc:runtime-facts.md].

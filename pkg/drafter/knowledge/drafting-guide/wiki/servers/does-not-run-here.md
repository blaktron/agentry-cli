---
title: Does not run here
type: list
summary: Declared MCP servers seen to fail in the runner, and packages whose presence as a direct dependency means a server cannot run, each with the reason, the runtime and the date. Generated from servers.yaml; do not edit by hand.
sources: [doc:servers.yaml]
updated: 2026-10-05
---

# Does not run here

These servers were seen to fail in the runner, or cannot run here for the reason each entry gives, on the runtime and the date it gives. Do not pin them: no setting of the QuickAgent makes them run. An entry comes off the list when the runner gains what the server lacked. A server not listed here can still fail; check it against [Declared MCP servers](mcp-servers.md) before you pin it. An entry speaks for every version of its server, unless its reason is `broken-upstream`, `invalid-tool-schema` or `runtime-excluded`, which hold for the version seen. The check after you answer sends a pin this page covers, or a server with a dependency listed at its end, to a repair turn. [src: doc:servers.yaml]

24 servers.

## npm `@aryanbv/pdf-toolkit-mcp`

- **Versions:** 0.3.1, on Node 24.21.0.
- **Why it does not run:** it works on local files or folders, and none of the run's files are in its container (`needs-run-files`).
- **Seen:** Started and listed its tools; a call of pdf_get_metadata for /inputs/report.pdf returned FILE_NOT_FOUND. Every tool takes a local file path, and none of the run's files are in its container. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@azure/mcp`

- **Versions:** 3.0.0-beta.49, on Node 24.21.0.
- **Why it does not run:** it needs a native binary built for glibc, and the Node runtime image is Alpine (musl) (`musl-native`).
- **Seen:** Installed; at start it failed to run its bundled program (spawn …/azmcp ENOENT): azmcp is built for glibc (its interpreter is /lib64/ld-linux-x86-64.so.2), and the runtime image is musl. It is also started with a subcommand (server start), which the runner does not pass. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@bytebase/dbhub`

- **Versions:** 1.4.0, on Node 24.21.0.
- **Why it does not run:** it talks to its service on a port other than 443 (a database, a local socket), and declared hosts are reached on 443 only (`non-443-port`).
- **Seen:** Installed; exited at start asking for a database connection (--dsn, --demo, or DSN). A DSN can be given in env, but the databases it connects to listen on their own ports (5432, 3306, …), which a declared server cannot reach. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/sdk`

- **Versions:** 1.32.1, on Node 24.21.0.
- **Why it does not run:** it is not an MCP server: it has no program to start (`not-a-server`).
- **Seen:** Not resolved: the package declares no bin. It is the MCP TypeScript SDK, a library for writing servers, and was declared by a published dev release as if it were a server. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-filesystem`

- **Versions:** 2026.8.31, on Node 24.21.0.
- **Why it does not run:** it works on local files or folders, and none of the run's files are in its container (`needs-run-files`).
- **Seen:** Started and listed its tools. Its tools read and write local directories, and none of the run's files are in its container. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-gdrive`

- **Versions:** 2025.1.14, on Node 24.21.0.
- **Why it does not run:** it signs in through a browser window or a credentials file of its own, and a package server gets only its environment and, with a sign-in the drafting guide lists, a token folder (`oauth-browser`).
- **Seen:** Installed; exited at start: Credentials not found. Please run with 'auth' argument first. It signs in through a browser and keeps a credentials file. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-gitlab`

- **Versions:** 2025.4.25, on Node 24.21.0.
- **Why it does not run:** it lists a tool whose input schema the runner's gateway refuses, which stops the gateway and every server in the run with it (`invalid-tool-schema`).
- **Seen:** Installed and started, and the runner's gateway stopped with a panic reading its tools (create_or_update_file: input schema must have type "object"), so no server in the run would have been served. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-postgres`

- **Versions:** 0.6.2, on Node 24.21.0.
- **Why it does not run:** it needs arguments or flags to start, and a server is started with none (`needs-arguments`).
- **Seen:** Installed; exited at start: Please provide a database URL as a command-line argument. The database would also be on port 5432, which a declared server cannot reach. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-puppeteer`

- **Versions:** 2025.5.12, on Node 24.21.0.
- **Why it does not run:** it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`).
- **Seen:** Started and listed its tools; a call of puppeteer_navigate failed: Could not find Chrome. Puppeteer downloads its browser in an install script, which does not run here, and the container has no browser. Archived by its authors. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@modelcontextprotocol/server-redis`

- **Versions:** 2025.4.25, on Node 24.21.0.
- **Why it does not run:** it talks to its service on a port other than 443 (a database, a local socket), and declared hosts are reached on 443 only (`non-443-port`).
- **Seen:** Installed; at start it tried redis://localhost:6379 and kept retrying. Redis listens on its own port (6379), which a declared server cannot reach, and the URL is a command-line argument. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@playwright/mcp`

- **Versions:** 0.0.83, on Node 24.21.0.
- **Why it does not run:** it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`).
- **Seen:** Started and listed its tools; a call of browser_navigate failed: Chromium distribution 'chrome' is not found. The container has no browser. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `@wonderwhy-er/desktop-commander`

- **Versions:** 0.2.52, on Node 24.21.0.
- **Why it does not run:** it works on local files or folders, and none of the run's files are in its container (`needs-run-files`).
- **Seen:** Started and listed its tools. Its tools run shell commands and edit local files, and none of the run's files are in its container; the shell it reaches is the server container's own. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `agentation-mcp`

- **Versions:** 1.3.2, on Node 24.21.0.
- **Why it does not run:** it needs arguments or flags to start, and a server is started with none (`needs-arguments`).
- **Seen:** Installed; started without the server subcommand it needs and wrote text, not MCP, to its output, so the gateway could not initialize it. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `chrome-devtools-mcp`

- **Versions:** 1.10.1, on Node 24.21.0.
- **Why it does not run:** it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`).
- **Seen:** Started and listed its tools; a call of new_page failed to launch Chrome. The container has no browser, and the server's container runs as root, which Chrome's sandbox refuses. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `mongodb-mcp-server`

- **Versions:** 3.0.5, on Node 24.21.0.
- **Why it does not run:** it talks to its service on a port other than 443 (a database, a local socket), and declared hosts are reached on 443 only (`non-443-port`).
- **Seen:** Started and listed its tools. Its database tools connect through a MongoDB connection string on port 27017, which a declared server cannot reach. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## npm `next-devtools-mcp`

- **Versions:** 0.4.0, on Node 24.21.0.
- **Why it does not run:** it talks to its service on a port other than 443 (a database, a local socket), and declared hosts are reached on 443 only (`non-443-port`).
- **Seen:** Started and listed its tools. It connects to a running Next.js dev server on the same machine, which a declared server cannot reach. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## PyPI `blender-mcp`

- **Versions:** 2.0.0, on Python 3.12.14.
- **Why it does not run:** it talks to its service on a port other than 443 (a database, a local socket), and declared hosts are reached on 443 only (`non-443-port`).
- **Seen:** Started and listed its tools. They work by talking to the Blender add-on's socket on localhost:9876, which a declared server cannot reach. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## PyPI `lean-lsp-mcp`

- **Versions:** 0.31.0, on Python 3.12.14.
- **Why it does not run:** it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`).
- **Seen:** Started and listed its tools. They run Lean's own programs (lake, the language server) on a local project, and the container has neither. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## PyPI `mcp-for-blender`

- **Versions:** 2.1.8, on Python 3.12.14.
- **Why it does not run:** it talks to its service on a port other than 443 (a database, a local socket), and declared hosts are reached on 443 only (`non-443-port`).
- **Seen:** Started and listed its tools. They work by talking to the Blender add-on's socket on the same machine, which a declared server cannot reach. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## PyPI `mcp-grafana`

- **Versions:** 2.0.0, on Python 3.12.14.
- **Why it does not run:** it writes into its own installed files, which are read-only (`writes-own-tree`).
- **Seen:** Installed; exited at start with OSError: Read-only file system: it marks its bundled program executable inside its own installed files, which are read-only. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## PyPI `mcp-server-git`

- **Versions:** 2026.8.18, on Python 3.12.14.
- **Why it does not run:** it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`).
- **Seen:** Installed; exited at start with ImportError: Bad git executable. It works through GitPython, which runs the git program, and the container has none. It also works on local repositories, which are not in its container. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## PyPI `mcp-server-sentry`

- **Versions:** 2025.1.14, on Python 3.12.14.
- **Why it does not run:** it fails at start on any machine, with the dependencies it installs today (`broken-upstream`).
- **Seen:** Installed; exited at start with ModuleNotFoundError: No module named 'httpx'. It imports a package it does not declare, which its dependencies no longer bring in. Archived by its authors; @sentry/mcp-server is on Runs here. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## PyPI `mcp-server-sqlite`

- **Versions:** 2025.4.25, on Python 3.12.14.
- **Why it does not run:** it fails at start on any machine, with the dependencies it installs today (`broken-upstream`).
- **Seen:** Installed; exited at start with AttributeError: 'Server' object has no attribute 'list_resources'. Its unpinned mcp dependency installs a version whose API it no longer matches. It also works on a local database file. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## PyPI `postgres-mcp`

- **Versions:** 0.3.0, on Python 3.12.14.
- **Why it does not run:** it fails at start on any machine, with the dependencies it installs today (`broken-upstream`).
- **Seen:** Installed; exited at start with ModuleNotFoundError: No module named 'mcp.server.fastmcp': its unpinned mcp dependency installs mcp 2.x, which renamed that module. The database would also be on port 5432. [src: doc:servers.yaml]
- **Evidence:** survey 2026-10-05.

## Dependencies that mean a server does not run here

A server whose own direct dependencies (npm's `dependencies`, PyPI's `requires_dist`) include one of these packages does not run here, whatever list the server itself is on [src: doc:servers.yaml].

- npm `playwright`: it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`). It needs a browser, which the container does not have; @playwright/mcp could not navigate. (survey 2026-10-05.)
- npm `playwright-core`: it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`). It needs a browser, which the container does not have; it is the core of the playwright package, which @playwright/mcp uses. (survey 2026-10-05 (inferred).)
- npm `puppeteer`: it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`). It needs a browser, which it downloads in an install script that does not run here; @modelcontextprotocol/server-puppeteer could not navigate. (survey 2026-10-05.)
- npm `simple-git`: it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`). It runs the git program, which the container does not have. No surveyed server depends on it; listed for the cause GitPython showed. (survey 2026-10-05 (inferred).)
- PyPI `gitpython`: it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`). It runs the git program, which the container does not have; mcp-server-git exited at start. (survey 2026-10-05.)
- PyPI `playwright`: it runs another program (a browser, git, a CLI), and its container holds only its runtime and a shell (`spawns-binary`). It needs a browser, which the container does not have. No surveyed server depends on it; listed for the cause the npm package showed. (survey 2026-10-05 (inferred).)

# MCP server survey

This program installs and starts declared MCP servers the way the runner does
and records what each one did. Its results seed the Drafter's two soft lists,
*Runs here* and *Does not run here*, in
`pkg/drafter/knowledge/drafting-guide/sources/servers.yaml` (plan
`drafter-knowledge.md` §4.5, agentry-cli#658). This README is the procedure:
follow it to run the survey again.

## What it does with each server

1. **Picks the version.** It takes the newest version whose own runtime range
   (npm's `engines.node`, PyPI's `requires_python`) admits the default runtime
   (Node and Python as `policy.DefaultNodeVersion` and `DefaultPythonVersion`
   give them). It starts from the registry's latest and works down through the
   releases, and skips pre-releases unless the latest is one. Each newer
   version it passes over is recorded under `excluded`, with the reason. A
   `version:` in the list pins one instead.
2. **Installs it with the runner's own builder.** It calls
   `mcpinstall.ResolveAll` and `mcpinstall.EnsureInstalled` with the default
   policy, so the install runs in the default runtime image, behind the install
   proxy, with `--ignore-scripts` for npm and wheels only for PyPI. The
   builder's output goes to the server's log.
3. **Starts it through the runner's gateway.** It runs the pinned MCP gateway
   image with the catalog entry a run renders
   (`compile.DeclaredServerGatewayFiles`) and a run's gateway flags
   (`--block-network`, `--block-secrets`, the container limits). The server
   therefore starts from its read-only store, with no arguments, as the
   image's own user, and reaches each declared egress host through a TCP relay
   on port 443. The survey's gateway differs from a run's in three ways, each
   looser, so a server that fails here fails in a run too, and one that
   starts here could still meet a refusal in a run: it speaks stdio, so the
   survey can talk MCP to it, where a run's agent reaches the gateway over
   streaming HTTP; it reaches the Docker daemon's own socket, where a run's
   gateway on Linux goes through the run's filter; and it uses the daemon's
   default runtime, where a hosted run's may be gVisor.
4. **Lists its tools and makes one call.** It sends `initialize` and
   `tools/list`, then makes the list's `call` if one is given. A call is given
   only for a read-only tool that needs no key. Without one, the result says
   "started and listed N tools, not called". A declared secret is given a
   placeholder value (`survey-placeholder-not-a-key`), so a server that checks
   for a key at start can still start and list its tools. The survey never
   uses a real key.

It also records, once per run, what a server starts as in each default runtime
image: the user, HOME, whether HOME and `/tmp` are writable, and which of the
programs servers often spawn (git, curl, a browser, Docker) the image has
(`runtimes-<date>.txt`).

## Running it

Run it on the workstation from `~/scratch`, never from a worktree: it writes
server stores, logs and results into its output folder, and refuses an
output folder inside a git work tree. It needs Docker and
network access to npm, PyPI, ghcr.io and the hosts the list declares. It runs
one server at a time and refuses to start a server while another gateway's
relays are on the host's shared relay network.

```bash
cd ~/agentry/.worktrees/agentry-cli/<branch>    # or the agentry-cli checkout
GOWORK=off go build -o ~/scratch/mcp-survey/mcp-server-survey ./scripts/mcp-server-survey
cp scripts/mcp-server-survey/list.yaml ~/scratch/mcp-survey/
cd ~/scratch/mcp-survey && ./mcp-server-survey -list list.yaml -out .
```

The 50 take about an hour on the workstation. It writes:

- `results-<date>.json`: one record per server (version, excluded versions,
  direct dependencies, npm install scripts, the furthest stage reached, the
  outcome, the error, the tools, the call and its excerpt);
- `results-<date>.md`: the same as a table;
- `runtimes-<date>.txt`: the runtime images' user, HOME and programs;
- `logs/<server>.log`: the builder's output and the gateway's log;
- `store/`, `gateway/`: the server stores and gateway configuration, which can
  be deleted afterwards.

`-only a,b` surveys only those packages and replaces their records in that
day's results. Use it after adding calls to the list for a second pass.

## From results to `servers.yaml`

The results are evidence; `servers.yaml` is written by hand from them, because
an entry needs a judgement the program cannot make: why a server failed.

- A server that started and listed its tools goes on *Runs here*. Its entry
  gives the version range seen, the runtime, the tools, what a QuickAgent must
  declare for it (its keys as `secrets`, its hosts as `egress`, an `env`
  switch) and what the call showed. When the server works only on local files,
  needs a non-443 port, or spawns a program to do its work, it goes on *Does
  not run here* even though it started: listing tools is not doing the task.
- A server that failed goes on *Does not run here* with one reason from the
  closed set in `pkg/drafter/serverlist.go` (`notRunReasons`), read from its
  log: a missing musl wheel or a native module is `musl-native`, a usage
  message at start is `needs-arguments`, and so on.
- A package that made a server fail goes under `dependencies` when any server
  depending on it directly would fail the same way (`onnxruntime`, a browser
  automation library).
- Every entry says what was observed, on which runtime, and on which date,
  with `evidence: survey <date>`. It never says a server is safe or good.

Then regenerate the two pages and check them:

```bash
GOWORK=off go test ./pkg/drafter -run TestTheDraftingGuideIsCurrent -update
GOWORK=off go test ./pkg/drafter
```

Any change to the pack moves the Drafter's tree digest, so it is a new Drafter
version with its own `builtins.lock` line (see `pkg/drafter/README.md`).

## The list

`list.yaml` holds the 50 servers of the 2026-10-05 survey, chosen as plan §4.5
says: the union of the sources below, topped up to 50.

1. **Every server a published QuickAgent declares.** On dev, this comes from
   the registry's own table (`agent_version_mcp_servers` joined to published
   versions, read on the dev host). Production's database is not reachable
   from the workstation, so production's are read from the public API instead
   (`/api/v1/search`, each agent's versions, each version's manifest). That
   covers public QuickAgents only. On 2026-10-05 the two gave 7 servers.
2. **The MCP reference servers**
   (https://github.com/modelcontextprotocol/servers): the seven current ones,
   and twelve of the archived ones. `server-aws-kb-retrieval-server` is no
   longer on npm (404 on 2026-10-05), so it was dropped. 19 servers.
3. **The most-downloaded MCP servers.** For npm, the packages that npm's search
   API returns for `keywords:mcp-server`, `keywords:mcp`, `mcp server` and
   `modelcontextprotocol`, ranked by last month's downloads
   (`api.npmjs.org/downloads`). For PyPI, the projects with `mcp` in their name
   in the top-PyPI-packages dataset
   (https://hugovk.github.io/top-pypi-packages/), ranked by 30-day downloads.
   It takes, in rank order, packages whose own description says they are an
   MCP server and that are not already on the list. It skips SDKs, frameworks,
   clients, proxies, instrumentation, editor add-ons, and tools whose MCP server
   is a side entry. That gave the top 14 on npm and the top 10 on PyPI.

To refresh it, repeat each step and replace the entries whose source moved.
Keep the `why:` of each entry.

# RUNNER-PLAN — the SecureAgentry Runner framework

**Status:** IMMUTABLE once M-R0 ships. Corrections go to `RUNNER-DEVIATIONS.md`, never here.
**Research annex:** `../agentry-framework-research.md` (2026-08-30) and its appendices — every
"why" in this plan is argued there with primary sources; this document states the "what".
**Registry contract:** `agentry/PLAN.md` §5 (manifest v1, capabilities §5.4, install targets
§5.6). This plan consumes that contract; it never widens it.

---

## R0. Mission

The registry half of SecureAgentry exists: immutable, scanned, DSSE-signed releases of
single-purpose agents, verified on install. This plan adds the **runner** — the consuming
runtime PLAN §5.4(d) promised, the thing that turns a declared capability contract into
actual enforcement:

```
agentry run ns/name@^1.2 --input task.json   →   result on stdout, reports in an
                                                 output folder, receipts, exit code
```

One agent, one job, one container tree, then gone. Five commitments (the operator
requirements this framework is built to):

- **RQ1 — self-contained tool plane.** Agents carry *no tool code*. Every tool any agent can
  use is served by a **companion MCP server** this framework ships (`agentry-runner-tools`),
  exposed through a Docker MCP Gateway. The agent package is prompt + manifest, nothing else.
- **RQ2 — two consumption modes, one contract.**
  *Standalone:* `agentry run` ensures the docker-mcp-gateway is present (pulled/pinned on
  first agent use) and serves tools through it.
  *Embedded:* an agent installed into an existing coding agent (Claude Code, Codex, …) via
  the §5.6 targets uses that host's **built-in** tools; the manifest capabilities compile to
  the host's tool-allowlist vocabulary instead. Same package, same contract, no bundled tools
  in either mode.
- **RQ3 — isolated one-shot containers with an output folder.** Agent code and prompts
  execute only in containers. Every run mounts exactly one host-visible output folder for
  reports and receipts. Nothing else the run wrote survives teardown.
- **RQ4 — generic agent format.** The runner translates *any* conforming package
  (manifest kind + prompt files) into the harness's native definition. No per-agent code,
  no per-agent images, no runner releases to onboard a new agent.
- **RQ5 — install-time configuration.** The gateway file set, harness definition, and
  container specs are compiled and persisted at **install time** (first `agentry run` of a
  digest, or `agentry run --prepare`). A run executes the installed plan; it does not
  re-derive it.

Unix rules carry over from the research annex §1: task in on argv/`--input`/stdin, result on
stdout, diagnostics on stderr, meaningful exit codes (§R8), no daemon — the gateway lives
exactly as long as the run.

## R1. What the runner consumes (already built — reuse, never reimplement)

- `pkg/manifest` (core module): parse/validate, `ParseRef`, the capability vocabulary.
- `pkg/apiclient` + `cmd/agentry/client.go`: resolve, download.
- `pkg/trust` + `pkg/attest`: DSSE verification against TOFU-pinned keys. **A run is gated
  exactly like an install**: digest must match, attestation must verify; unsigned requires
  `--allow-unsigned`; mismatch/unverifiable always aborts.
- `pkg/archive`: hardened extraction (traversal, bombs, symlinks refused).
- `pkg/installed`: capability snapshot/diff; run-time confirmation reuses install semantics
  (egress/exec or a widened contract needs `--yes` non-interactively).

## R2. The agent definition (generic format, RQ4)

A runnable package is manifest v1 plus prompt files. The runner derives the harness
instruction deterministically:

| kind | instruction source | notes |
|---|---|---|
| `skill` | body of `entrypoints.skill` (frontmatter stripped) | the §5.6 claude-skill entrypoint, reused |
| `prompt-agent` | `AGENT.md` at archive root; else `PROMPT.md`; else compile error naming both | rule is closed — no guessing beyond these two names |
| `mcp-server` | not runnable as an agent (exit 12) | it is a *tool*, attachable to other agents' runs (backlog, DR-5) |

Optional, by convention (documented, not in the manifest schema — the registry schema is
frozen): `output.schema.json` at the archive root declares the agent's structured result; the
runner wires it into the harness's schema-enforced output mode and validates before exit
(§R8 code 17).

The compiler appends a fixed **runner contract postscript** to every instruction (verbatim in
`pkg/runner/compile`, golden-tested): tools come only from the MCP gateway; the workspace is
at `/workspace` *as seen by tools*; report files must be written with `write_report`; the
final message is the result.

Task input precedence (borrowed from axe, annex §4.2): `--input file` > stdin > empty task.

## R3. The companion tools server (RQ1)

`agentry-runner-tools` is one MCP stdio server, implemented in `runnerimage/toolsrv`
(stdlib-only Go), launched **by the gateway** as an isolated container from the locally built
runner image. Its tool surface is constructed from the manifest at compile time — an
undeclared capability means the tool is *absent*, not refused:

| tool | present when | behavior & enforcement |
|---|---|---|
| `fs_read`, `fs_list` | `filesystem.read` ≠ none/undeclared | read under `/workspace`; `paths` mode additionally restricted server-side to the declared list; mount itself is `:ro` unless write=workspace |
| `fs_write` | `filesystem.write` = workspace or paths | write under `/workspace` (paths mode: only declared paths); mount `:rw` |
| `fetch` | `network.egress` is a host list | HTTP(S) to declared hosts only, checked server-side **and** enforced at container level by gateway `allowHosts` + `--block-network`; optional `auth` param names a declared secret env var — the tool injects the header itself so the value never enters model context |
| `run_command` | `exec.commands` non-empty | argv[0] must be a declared command; executed with `PATH` set to a shim dir containing only declared commands, cwd `/workspace` (DR-1 discloses the residual: the image is not binary-minimal in v1) |
| `write_report` | always | write under `/out/reports/`; traversal-safe; this is the RQ3 output channel |

Path handling everywhere: clean, reject `..` escape, reject symlink escape, byte-capped
reads/writes. Every tool call is already logged by the gateway (`--log-calls`); the log is
collected into receipts.

Registry-hosted `kind: mcp-server` dependencies as additional gateway backends: **deferred**
(DR-5); `dataAccess` categories map to operator-cataloged servers via policy
(`policy.dataAccessServers`), default empty.

## R4. Capability → enforcement (the compiler's law)

Every manifest line maps to a concrete, testable control; every control names its mechanism
in the receipt (honest-posture rule). Compiled per digest at install time into the **run
plan** — deterministic, hashable, printable with `--dry-run`.

| manifest | compiled control | level |
|---|---|---|
| `network.egress: none` / undeclared | tools container `disableNetwork: true`; no `fetch` tool | hard (`--network none`) |
| `network.egress: [hosts]` | tools container `allowHosts: [host:443, …]`; gateway **always** launched with `--block-network` (asserted in code and by test — without it allowHosts is advisory) | hard |
| `network.egress: "*"` | refused unless policy `allowWildcardEgress: true`; then tools container gets unrestricted egress and the receipt says so | policy gate |
| `filesystem.*` | mount table on the **tools** container only (`:ro`/`:rw`, per-path binds for `paths` mode); the agent container mounts no workspace at all | hard |
| `exec: none` / undeclared | no `run_command` tool; harness built-in shell/fs toolsets are never enabled in any case | hard (tool absent) |
| `exec: {commands}` | `run_command` + shim PATH as §R3 | hard on argv0/PATH; DR-1 residual disclosed |
| `secrets: [NAMES]` | resolved from the operator secret source at run time; **missing ⇒ exit 11 before any container starts**; written to a per-run `secrets.env` (0600, deleted on teardown); gateway injects each only into the tools container (`secrets:` entries); never into the agent container, never into receipts | hard scoping + `--block-secrets` scrubbing |
| `dataAccess: [cats]` | selects policy-cataloged servers; recorded in receipt | policy + audit |

**Agent-container topology (identical for every run):** one run-scoped bridge network created
`--internal` (no route out). The agent container attaches only to it and can reach exactly
two names: `gateway:8811` (Bearer-token MCP endpoint) and `llm-proxy:3128` (an allowlisting
CONNECT proxy from `runnerimage/netproxy`; allowlist = `policy.model.proxyAllow`, i.e. model
endpoints only). `HTTPS_PROXY` points at the proxy, `NO_PROXY=gateway`. No docker socket, no
workspace, no out-dir, no manifest secrets — the only secret in the agent container is the
operator's model API key, which the receipt discloses. Manifest egress hosts are *tool-plane*
grants and never widen the agent container.

**Gateway container:** `docker/mcp-gateway` pinned by policy, `/var/run/docker.sock`
mounted (the isolation boundary is the docker daemon — documented, not hidden), generated
config mounted read-only at `/mcp`, `MCP_GATEWAY_AUTH_TOKEN` = per-run random token,
`MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS`/`…_ALLOW_WRITABLE_PATHS` pinned to exactly the run
dir's workspace/out paths. Flags hard-wired by the launcher: `--block-network`,
`--verify-signatures`, `--log-calls`, `--transport streaming --port 8811`, `--servers` from
the plan. Dynamic tool discovery stays off.

## R5. Modes (RQ2)

**Standalone (`agentry run`).** First use of a digest = install: verify → materialize
workspace → compile plan → `docker pull` the pinned gateway + harness images → `docker build`
the runner image from the embedded context (§R7). Subsequent runs reuse
`~/.agentry/runner/agents/<ns>/<name>/<version>-<digest12>/`. `--prepare` stops after
install; `--local DIR` runs an unpublished package directory (validated, treated as unsigned:
requires `--allow-unsigned`, receipts say `local`).

**Embedded (§5.6 install targets, extended).** `agentry install --target claude-skill`
additionally compiles capabilities into spec-conformant `allowed-tools` frontmatter
(`Read/Grep/Glob` ⇐ fs.read, `Write/Edit` ⇐ fs.write, `Bash(<cmd>:*)` ⇐ exec commands,
`WebFetch(domain:<host>)` ⇐ egress hosts); `--target agents-md` appends the same contract as
a "Tools" note in the section. The host coding agent supplies the tools; nothing is bundled.
Enforcement level is the host's (declared-vs-enforced table ships in docs).

## R6. Run lifecycle & layout (RQ3, RQ5)

```
~/.agentry/runner/
  policy.yaml                          # operator policy (§R9)
  image/                               # materialized embedded build context
  agents/<ns>/<name>/<ver>-<digest12>/
    workspace/                         # verified extracted archive (pristine)
    plan/
      plan.json                        # the run plan, canonical JSON (§R10), sha256-hashed
      gateway/{catalog,registry,config,tools}.yaml
      harness/…                        # adapter-native files (e.g. agent.yaml)
<out dir>/                             # --out, default ./agentry-out/<ns>-<name>-<runid>/
  result.json                          # the result envelope (§R8) — also stdout
  reports/…                            # whatever the agent wrote via write_report
  receipts/{events.ndjson, gateway-calls.log*, run-plan.json, meta.json}
```

Per run: fresh runid; ephemeral run dir under the install dir (`runs/<runid>/`, deleted on
teardown) holding the rw workspace copy and `secrets.env`. Execute order: create network →
start llm-proxy → start gateway (health: token-authed MCP initialize probe) → start agent
container (harness, task on stdin) → stream NDJSON events to receipts (enforcing
`--max-turns` by event count, `--timeout` by deadline) → collect result → teardown
containers, network, secrets file — teardown runs on every path, success or failure, and
`docker ps`-level leak checks are part of acceptance.

## R7. The runner image (self-containment, RQ1)

`runnerimage/` in this repo is both a normal Go package tree (unit-tested here) and an
embedded docker build context: `Dockerfile` (golang builder stage → scratch-ish runtime) +
`main.go` + `toolsrv/` + `netproxy/`, all **stdlib-only** (enforced by test). First use
materializes it to `~/.agentry/runner/image/` and builds `agentry-runner:<version>` locally —
no registry of ours required, nothing downloaded but the pinned public golang/gateway/harness
images. The same binary serves `tools` (MCP stdio) and `proxy` (CONNECT allowlist) roles.

## R8. Exit codes & result envelope

`0` ok · `1` CLI/usage · `10` verify failed · `11` policy refused / missing secret ·
`12` compile · `13` gateway or docker environment · `14` harness/agent runtime ·
`15` timeout · `16` budget (turns) · `17` structured-output validation exhausted.

Envelope on stdout (`--output-format json`; `text` prints only the result text and writes the
envelope to `result.json` regardless): `agentry: "run/v1"`, ref, digest, attestation
{keyid, verified}, capabilities_enforced (with mechanism strings), harness {adapter,
image, model}, usage {tokens, cost if reported, wall_ms, turns}, result, out_dir, receipts
{run_plan_sha256}. Pipes must compose: `agentry run a … | agentry run b … ` (M-R5 acceptance).

## R9. Operator policy (`~/.agentry/runner/policy.yaml`)

Versioned (`policyVersion: "1"`), closed schema. Fields: `images` (gateway/harness/builder
pins — tag or digest; docs recommend digests), `model` {adapter's provider, model, apiKeyEnv,
proxyAllow []host:port}, `allowWildcardEgress` (default false), `secretSource` {env |
envFile path}, `dataAccessServers` map, `receiptsKeep` — defaults are written on first use
with loud comments. Policy participates in the plan hash: a policy change invalidates
installed plans (recompiled on next run, capability re-confirmation rules apply).

## R10. Adapter interface (harness independence)

```go
type Adapter interface {
    Name() string
    // Compile renders harness-native definition files into plan/harness/ and
    // returns the agent-container spec (image, argv, env, mounts, stdin mode).
    Compile(p *spec.RunPlan, dir string) (spec.HarnessSpec, error)
    // Events parses one harness output line into zero or more normalized
    // runner events (turn, usage, result, error).
    Events(line []byte) []Event
}
```

Default adapter: **docker-agent** (annex §5 verdict). The adapter renders `agent.yaml`
(single root agent; `toolsets: [{type: mcp, remote: {url, transport_type: streamable,
headers: {Authorization: Bearer ${env.MCP_GATEWAY_TOKEN}}}, allow_private_ips: true}]`; no
built-in toolsets, ever), runs `run --exec /plan/agent.yaml -` with `--json`, and maps NDJSON
to events. `goose`, `claude`, `codex` slots are registered names that error with "not
implemented in v1" (M-R4). Adapters never receive secret values — only env var names.

## R11. Security invariants (each one is a test)

1. Gateway is never launched without `--block-network`. (unit + M-R3 negative test)
2. Agent container joins only the internal run network; no socket, no workspace mount.
3. Manifest secrets appear only in `secrets.env` (0600) and the tools container env.
4. Undeclared capability ⇒ corresponding tool absent from `tools/list`. (toolsrv unit)
5. `write_report`/`fs_*` cannot escape their roots (traversal corpus).
6. Teardown removes containers, network, and `secrets.env` on every exit path.
7. Receipts never contain secret values or the gateway token.
8. Every enforcement claim in the envelope names its mechanism.

## R12. Milestones

- **M-R0** spec (this file) + `pkg/runner/spec` + fixtures + `--dry-run` golden acceptance
  (`scripts/acceptance/runner-r0.sh`, no docker).
- **M-R1** compiler + policy + adapters(docker-agent) + gateway file set, deterministic +
  hashed, golden-tested across the capability matrix.
- **M-R2** execute path on plain docker CE (engine, gateway launcher, runner image,
  budgets, receipts, teardown), fake-docker unit coverage; `runner-r2.sh` E2E gated on a
  docker host + model key.
- **M-R3** enforcement negatives (undeclared egress blocked, exec shim, ro workspace,
  missing secret ⇒ 11, gateway death ⇒ 13) as `testdata/runtime-corpus/` + script.
- **M-R4** goose/claude adapters + conformance diffing; empirical verification of proxy
  honoring and NDJSON schemas.
- **M-R5** pipe/cron acceptance, `--pin` + offline cache, mcp-server dependency backends,
  receipts retention, docs site page.

Non-goals of v1 (recorded so they aren't silently dropped): K8s executor, serve/A2A modes,
run-result attestations to the registry, house harness, wassette exec backend, cost hard-cap.

## R13. Decisions on the annex's open questions

Q-R1 wildcard egress: policy-gated, default refuse. Q-R2 model endpoints: explicit policy
list, key operator-owned only. Q-R3 secret source: env / env-file in v1, pluggable later.
Q-R4 default adapter: docker-agent. Q-R5 receipts: local dir only. Q-R6 naming:
`agentry run` in this CLI; product name "SecureAgentry Runner".

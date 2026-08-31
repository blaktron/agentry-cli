# SecureAgentry Runner — as-built architecture

**Status:** LIVING document — updated as the code moves. The immutable design record is
`RUNNER-PLAN.md` (v1) and `RUNNER-PLAN-2.md` (v2); divergences live in
`RUNNER-DEVIATIONS.md`. This page describes what is actually in the tree today so nobody
has to reconstruct it from four documents and a diff.

## What this repo is

The SecureAgentry CLI: the registry client (`init`/`validate`/`publish`/`install`/
`verify`/`search`/`info`/`yank`) plus the **BriefAgent runner** — the consuming runtime
that turns a release's declared capability contract into actual enforcement and executes
it once, in an isolated container, with an output folder for reports.

Two consumption modes, one contract:

- **Standalone** (`agentry run`): tools are served through a per-run Docker MCP Gateway
  fronting the companion tools server this repo embeds. BriefAgents carry no tool code.
- **Embedded** (`agentry install --provider claude|codex|openclaw`, or `--target`):
  the host application's own MCP servers and built-in tools stand in; the contract
  compiles to that host's allowlist vocabulary (`pkg/export/hosttools.go`).

## Package map

```
cmd/agentry            the CLI (package main): command wiring, registry I/O glue,
                       verify gate, install/run orchestration entry points
pkg/runner/spec        the run plan: deterministic, hashable compiled artifact;
                       placeholders (@@RUN_*@@) for run-scoped values
pkg/runner/policy      operator policy (~/.agentry/runner/policy.yaml): image pins,
                       model endpoint, budgets, secret source, profile defaults
pkg/runner/compile     the capability compiler: manifest+policy → plan + gateway file
                       set (catalog/registry/config/tools yaml + toolspec.json) +
                       harness definition; every contract line → one testable control
pkg/runner/adapter     harness adapters behind a 3-method interface; docker-agent is
                       the default; goose/claude/codex are registered M-R4 slots
pkg/runner/engine      execution: render placeholders, stage inputs, resolve secrets,
                       network+proxy+gateway+agent containers, budgets, receipts,
                       teardown on every path; classified exit codes
pkg/runner/dockerx     the docker CLI behind a small interface + a scriptable Fake
                       (the engine's whole lifecycle is unit-tested against it)
pkg/runner/gateway     first-use install: pull pinned gateway/harness images, build
                       the runner image from the embedded context
pkg/runner/profile     MCP profiles: per-signature stored keys/identities (§P2)
pkg/runner/setup       guided flows (machine setup §P1, profile setup §P3) behind a
                       Prompter interface — CLI TTY and desktop form share the logic
pkg/runner/receipt     the result envelope (§R8)
runnerimage/           the embedded docker build context (stdlib-only, enforced by
                       test): MCP tools server, CONNECT egress proxy, health probe;
                       materialized and built locally as agentry-runner:<version>
pkg/export             §5.6 exporters + embedded-mode host-tool mapping
pkg/installed          install-state + capability diff/consent bookkeeping
pkg/trust              DSSE attestation verification against TOFU-pinned keys
```

The desktop app (`blaktron/agentry-desktop`) imports `pkg/runner/*`, `pkg/export`,
`pkg/trust` and drives the same flows as forms — no parallel implementations. Treat
every exported symbol in those packages as API.

## One run, end to end

```
agentry run ns/name@^1 --input task.json --file brief.pdf --out ./reports
│
├─ MATERIALIZE   resolve → download → sha256 digest check → DSSE verify (pkg/trust;
│                unsigned needs --allow-unsigned; mismatch always aborts) → hardened
│                extract → cached per digest under agents/<ns>/<name>/<ver>-<digest12>/
├─ PLAN          reuse plan/ if {digest, policy sha, adapter} match and files hash
│                clean (compile.VerifyFiles); else recompile. --dry-run prints it.
├─ CONSENT       capability gate (egress/exec or widened ⇒ typed yes / --yes),
│                recorded in pkg/installed under the runner key
├─ PROFILE       load MCP profile for this signature; if secrets are declared and no
│                profile exists, guided setup on a TTY collects them (hidden input)
│                and asks keep/remove; secrets resolve profile > env/envFile
├─ ENSURE        docker reachable; pinned images pulled; runner image built (also
│                available up front via `agentry setup`)
├─ EXECUTE       run-scoped INTERNAL network
│      agent ctr ──MCP──▶ gateway ctr ──stdio──▶ tools ctr (+ dataAccess servers)
│      (harness,          (--block-network,      /workspace per fs contract,
│       /plan ro only)     Bearer token)         /inputs ro, /out rw, secrets scoped)
│      agent ctr ──CONNECT──▶ llm-proxy ctr (allowlist = policy model endpoints)
│                stream NDJSON → receipts; enforce --timeout / --max-turns
├─ COLLECT       result envelope → stdout (+ result.json); reports/ + receipts/
│                (events.ndjson, gateway.log, run-plan.json, meta.json, task.txt)
├─ TEARDOWN      containers, network, secrets file — every exit path — then a
│                label sweep (agentry.run=<id>); profile kept (touched) or removed
└─ EXIT          0 ok · 1 CLI · 10 verify · 11 policy/secret · 12 compile ·
                 13 gateway/docker · 14 agent · 15 timeout · 16 budget · 17 schema
```

## On-disk layout (`~/.agentry/runner`, override `AGENTRY_RUNNER_HOME`)

```
policy.yaml                                  operator policy (§R9 + §P1)
image/                                       materialized runner-image build context
agents/<ns>/<name>/<ver>-<digest12>/         verified install cache
  install.json                               ref/digest/attestation, written once
  workspace/                                 pristine extracted archive
  plan/{plan.json, gateway/*, harness/*}     the compiled plan + generated files
  runs/<runid>/                              per-run scratch (deleted at teardown):
    workspace/ inputs/ cfg/                  rw copy · :ro inputs · rendered config
    cfg/gateway/secrets.env                  0600, inside the gateway's /mcp mount
profiles/<ns>/<name>/<ver>-<digest12>/       MCP profiles (0700/0600)
```

## The tool surface (toolspec schema v2)

Compiled from the manifest into `plan/gateway/toolspec.json`; the tools server
registers exactly the granted subset — an undeclared capability's tool is **absent**:

| granted by | tools |
|---|---|
| always | `write_report`; `list_inputs`, `read_input` (the `--file` channel, :ro) |
| `filesystem.read` | `fs_read`, `fs_list` (paths mode restricts server-side too) |
| `filesystem.write` | `fs_write` |
| `network.egress` hosts | `fetch` (host allowlist re-checked per redirect hop; secrets injected server-side via `auth`) |
| `exec.commands` | `run_command` (argv0 allowlist + shim PATH) |

## Security invariants (each one is a test — see RUNNER-PLAN §R11)

Gateway never launched without `--block-network` · agent container: internal network
only, `/plan` mount only, no socket, no workspace, no manifest secrets · secret values
only in profile (0600), per-run secrets.env (0600, deleted), and the tools container
env — never in plans, receipts, argv, or process listings · teardown on every path +
label sweep · receipts name every enforcement mechanism (honest posture).

## Verification map

`make check` = gofmt + vet + golangci-lint + all tests + build. Acceptance:
`scripts/acceptance/runner-r0.sh` (plan matrix, determinism, policy gates, exit codes —
no docker), `runner-r1.sh` (live MCP stdio against the tools server), `runner-r2.sh`
(full docker E2E; self-skips without a daemon + model key; the standing open item,
DR-7). Engine/compile suites run `-race`. Golden plan files under
`pkg/runner/compile/testdata/golden/` are the framework's compatibility contract with
itself — regenerate with `-update` and review like an API change.

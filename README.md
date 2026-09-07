# agentry — the Agentry CLI

The command-line client for [Agentry](https://secureagentry.net):
publish, install, verify, and **run** BriefAgents — single-purpose AI
agents. Every install checks the archive digest and DSSE signature before
anything lands on your machine, and `agentry validate` runs the same
manifest rules and content scanners the registry runs at publish time —
offline. Start a new BriefAgent from the
[template](https://github.com/blaktron/agentry-template).

This repository holds the CLI only. It depends on the core module
[`github.com/blaktron/agentry`](https://github.com/blaktron/agentry) for the
package format (`pkg/manifest`), the scanners (`pkg/rules`, `pkg/ruleset`),
deterministic archives (`pkg/archive`), attestation verification
(`pkg/attest`), and the generated API client (`pkg/apiclient`).

## Building

```bash
make build          # -> bin/agentry
make check          # gofmt + vet + lint + tests + build
```

**Until the repositories are public** (the open core is private until the
operator flips it; see `PUBLIC-FLIP.md` in agentry-notes), the core module
cannot be fetched anonymously. Either work from a workspace with a `go.work`
that resolves `github.com/blaktron/agentry` against a sibling checkout, or:

```bash
export GOPRIVATE='github.com/blaktron/*' GONOSUMDB='github.com/blaktron/*'
# authenticate git for github.com/blaktron (gh auth, a credential helper,
# or an insteadOf rewrite with a read token), then make build
```

CI does the same with the `AGENTRY_BUILD_TOKEN` secret. All of this goes
away at the flip.

## The registry it talks to

`agentry` works against any Agentry registry — the hosted service or one you
run from the open core ([self-hosting guide](https://github.com/blaktron/agentry/blob/main/docs/self-hosting.md)).
The registry is chosen by `--registry`, else `defaultRegistry` in
`~/.config/agentry/config.yaml` (written by `agentry login`), else the
default compiled into the binary (`defaultRegistryURL`, set with
`-ldflags "-X main.defaultRegistryURL=https://…"` by whoever builds and
serves the binaries; a source build falls back to `http://localhost:8080`).
`agentry login` opens the registry's own `/ui/authorize` page, so sign-in
works the same whether or not a site fronts the server.

## Commands

`init` · `validate` · `login` · `publish` · `install` · `setup` · `run` · `uninstall` ·
`profiles` · `verify` · `search` · `info` · `yank` — run `agentry --help`
for the full surface. Server-side end-to-end coverage lives in the main
repo's `scripts/acceptance/m3-cli.sh`, which builds this CLI against a
live registry.

## The runner (`agentry run`)

The runner turns a release's declared capability contract into actual
enforcement and executes the BriefAgent once, in an isolated container,
with an output folder for its reports:

```bash
agentry run demo1/incident-summarizer@^1 --input incident.json --out ./reports
```

One run = verify (digest + DSSE, exactly like install) → compile the
manifest + operator policy into a deterministic run plan → execute:

- **Tools come only from a per-run [Docker MCP Gateway](https://github.com/docker/mcp-gateway)**
  fronting the companion tools server this CLI ships (built locally from an
  embedded context on first use). BriefAgents carry no tool code; an
  undeclared capability means the tool does not exist.
- **The agent container is isolated**: internal-only network whose sole
  reachable endpoints are the gateway (Bearer-token MCP) and an
  allowlisting egress proxy for the model API. No workspace mount, no
  docker socket, no manifest secrets.
- **Everything the run produces** lands in the output folder: the
  BriefAgent's report files, `result.json` (the machine-readable envelope),
  and `receipts/` (event stream, gateway log, the exact run plan).
  Containers, networks, and secret files are torn down on every exit path.
- Installed into an existing coding agent instead (`agentry install
  --target claude-skill|agents-md`), the same contract compiles to the
  host's built-in tool grants — nothing is bundled in either mode.

Exit codes: `0` ok · `10` verify · `11` policy/secret · `12` compile ·
`13` gateway/docker · `14` agent · `15` timeout · `16` budget ·
`17` output schema. Task input: `--input FILE`, else piped stdin. Runs
compose: `agentry run a ... | agentry run b ...`.

Around the enforcement core (v2): **`agentry setup`** configures your LLM
provider and stands up the gateway plane before any agent is involved;
**MCP profiles** store a BriefAgent's keys and identities once per exact
release (guided setup on first run; keep or remove on completion —
`--keep-profile`, `agentry profiles list|remove`); **`--file`** hands a
run external files read-only under `/inputs`; and
**`agentry install --provider claude|codex|openclaw`** installs for a
host agent application instead of the gateway path.

The operator policy (`~/.agentry/runner/policy.yaml`, created on first
use) pins images, names the model endpoint, and gates wildcard egress.
As-built architecture: [ARCHITECTURE.md](https://github.com/blaktron/agentry-notes/blob/main/ARCHITECTURE.md) §6
in agentry-notes. The original design records (RUNNER-PLAN, -2, -3) are archived there under
[archive/agentry-cli/](https://github.com/blaktron/agentry-notes/tree/main/archive/agentry-cli); divergences:
[RUNNER-DEVIATIONS.md](https://github.com/blaktron/agentry-notes/blob/main/agentry-cli/RUNNER-DEVIATIONS.md); acceptance:
`make accept-runner` (the docker E2E self-skips without a daemon).

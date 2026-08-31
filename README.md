# agentry — the SecureAgentry CLI

The command-line client for [SecureAgentry](https://secureagentry.net):
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

The core module is private; builders have access to all blaktron repos:

```bash
export GOPRIVATE='github.com/blaktron/*'
# authenticate git for github.com/blaktron (gh auth, a credential helper,
# or an insteadOf rewrite with a PAT), then:
make build          # -> bin/agentry
make check          # gofmt + vet + lint + tests + build
```

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
As-built architecture: [ARCHITECTURE.md](https://github.com/blaktron/agentry-notes/blob/main/agentry-cli/ARCHITECTURE.md). Immutable
specifications: [RUNNER-PLAN.md](https://github.com/blaktron/agentry-notes/blob/main/agentry-cli/RUNNER-PLAN.md) and
[RUNNER-PLAN-2.md](https://github.com/blaktron/agentry-notes/blob/main/agentry-cli/RUNNER-PLAN-2.md); divergences:
[RUNNER-DEVIATIONS.md](https://github.com/blaktron/agentry-notes/blob/main/agentry-cli/RUNNER-DEVIATIONS.md); acceptance:
`make accept-runner` (the docker E2E self-skips without a daemon).

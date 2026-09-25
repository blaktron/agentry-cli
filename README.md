# agentry — the Agentry CLI

The command-line client for [Agentry](https://secureagentry.net):
publish, install, verify, and **run** BriefAgents — single-purpose AI
agents. Every install checks the archive digest and DSSE signature before
anything lands on your machine. An unsigned release, one the registry
published no attestation for, requires `--allow-unsigned`. An unscanned
release is its own outcome: the registry admitted it without a scan (a
private organization agent its administrator took off the pipeline), so no
findings, no report and no attestation stand behind it. It requires
`--allow-unscanned`, on `install` and on every `run`. `agentry verify`
reports it as "unscanned, no attestation", and the run's receipt names the
consent that admitted it. `agentry validate`
runs the same manifest rules and content scanners the registry runs at submit
time — offline. Start a new BriefAgent from the
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

## Releasing

A release is a tag `v<major>.<minor>.<patch>` on a commit that has been the
head of `main` (a production release, baking `https://secureagentry.net`) or
of `dev` (a dev release, baking `https://dev.secureagentry.net`).
`build/release.env` names both registries and is the same on both branches;
`scripts/release-registry.sh` picks one by the commit's first-parent history
and refuses any other commit (agentry-cli#240). `agentry --version` prints the
registry a binary falls back to. A production release tags main's head:

```bash
git fetch origin && git tag v0.1.0 origin/main && git push origin v0.1.0
```

(plan `agentry-notes/plans/cli-release-signing.md`). A `workflow_dispatch`
of the `release` workflow from `main` is a dry run of the same job. The
workflow builds the five binaries with `make dist` (linux and darwin on amd64
and arm64, windows on amd64; the tag is the version, bare, and
the registry they talk to by default is the branch's, as above), signs the
Windows exe against Azure Artifact Signing in the same job — the job's
identity is the OIDC token GitHub mints for it in the `release` environment,
so it holds no Azure secret; `jsign` signs, `osslsigncode` verifies to the
Microsoft root pinned in `build/windows/signing/` and the CN `release.env`
names — writes `BUILD-INFO` and `SHA256SUMS`, signs those sums with the
estate's own update-signing key (the desktop's `signrelease -product
agentry-cli`, at the desktop commit the workflow pins: the signed comment
names this product, and the tool verifies what it wrote against the committed
key set), and uploads the set as one workflow artifact `agentry-cli-<version>`.
The site will import it with `build-downloads.sh --from-run
blaktron/agentry-cli v<version>`, verify both signatures, and say "signed" on
the Windows binary's card (agentry-web#214; until then the site still builds
the CLI itself). No GitHub Release before 1.0. A `workflow_dispatch`
run from `main` is a dry run: it signs exactly as a tag does and the site
refuses its bundle.

**Nothing in this repository depends on being signed** (plan D5): `make
build`, `make check` and `make dist` need no token, no certificate and no
signing tool; the CLI never inspects its own signature. The signing scripts
(`scripts/sign-windows.sh`, `scripts/signing-token.sh`,
`scripts/check-windows-binary.sh`) are the release workflow's, and
`scripts/acceptance/windows-signing.sh` proves that seam on every PR with a
throwaway CA in place of the key. The macOS binaries are signed and notarized
once the Apple credential exists (agentry-cli#198).

## The registry it talks to

`agentry` works against any Agentry registry — the hosted service or one you
run from the open core ([self-hosting guide](https://github.com/blaktron/agentry/blob/main/docs/self-hosting.md)).
The CLI takes the registry from `--registry`, else from `defaultRegistry` in
`~/.config/agentry/config.yaml` (written by `agentry login`), else from the
default compiled into the binary (`defaultRegistryURL`). Whoever builds and
serves the binaries sets that default with
`-ldflags "-X main.defaultRegistryURL=https://…"`; a source build falls back
to `http://localhost:8080`.
`agentry login` opens the registry's own `/ui/authorize` page, so sign-in
works the same whether or not a site fronts the server.

## Commands

`init` · `validate` · `login` · `whoami` · `publish` · `install` · `list` ·
`setup` · `doctor` · `run` · `uninstall` · `profiles` · `verify` · `search` ·
`info` · `yank` — run `agentry --help` for the full surface. `whoami` prints
the account the stored credential belongs to, the organizations it belongs
to with its role in each (a membership an admin has yet to approve says so)
and, where the registry has subscription tiers (its `/meta` lists the
billing extension), the tier and the credits — for a member of an
organization the tier is the organization's, the balance is its pool's, and
its seats and standing come with them. A `publish` the allowance cannot pay
for is refused with the registry's `402 credits-exhausted` sentence, the
account it is about, and the ordinary failure exit — the upgrade is the
site's, never the CLI's. With the root `--json`
flag the reading and writing commands print one JSON document on stdout
(`run` takes `--output-format json`), and `install --plan --json` /
`run --plan --json` print the consent-free preview a client shows a person
first. Server-side end-to-end coverage
lives in the main repo's `scripts/acceptance/m3-cli.sh`, which builds this
CLI against a live registry.

### Signing keys and `verify`

`agentry login` pins the registry's signing keys
(`/.well-known/agentry/signing-keys.json`) on first use, under
`~/.config/agentry/keys/<host>.yaml`, a file the desktop shares. From then
on, `install`, `run` and `verify` accept a signature only from a pinned key.
When a later login finds a different key set, it says what changed and
keeps the pin:

- **A possible key rotation.** The registry added a key and still
  advertises every pinned one. That is consistent with a rotation, which
  keeps retired keys listed, but the client cannot tell a rotation from an
  added key it should not trust, so confirm it with the registry's operator.
  Releases the pinned keys signed still verify. A release signed only by
  the new key is refused until you re-pin.
- **A warning.** A pinned key is gone, or a pinned key id now carries a
  different public key. That is not a rotation; treat it as a possible
  compromise.

In both cases you re-pin deliberately: delete the pin file and log in
again. The registry also records each key as `active` or `retired`, with
the date it retired. Login copies that record into the pin file. When the
key that verified a release is retired, `verify` prints `signed by a
retired key (retired <date>)`, and its `--json` report carries `keyRetired`
and `keyRetiredAt`. The key still verifies exactly as it did before.

## The runner (`agentry run`)

The runner turns a release's declared capability contract into actual
enforcement and executes the BriefAgent once, with an output folder for
its reports. The default adapter (`docker-agent`) runs the harness in an
isolated container; `--adapter claude` or `codex` runs your own installed
Claude Code or Codex as a host process behind the same gateway and egress
proxy (`goose` is planned, not implemented):

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
  docker socket, no manifest secrets. It does hold the model API key: the
  harness inside it calls the model, so the key is in its environment,
  under the variable that harness reads, and the receipt says so.
- **The containers the runner starts** (the proxy, the gateway, the
  probe, the agent, the MCP server installer) drop all capabilities, set
  no-new-privileges, and are bounded by the policy's `docker.limits` (2g
  of memory, 1 CPU and 512 processes unless the policy says otherwise).
  Two keep one capability back: the gateway `DAC_READ_SEARCH`, to read the
  run's secrets file, and an interactive session's store one-shot `CHOWN`.
  The agent's, the proxy's and the probe's root filesystem is read-only.
  The tool and MCP server containers the gateway launches get the same
  memory and CPU bounds through its flags, and its own no-new-privileges;
  their capabilities are the gateway's defaults. The receipt names what
  was applied.
- **Everything the run produces** lands in the output folder: the
  BriefAgent's report files, `result.json` (the machine-readable envelope),
  and `receipts/` (event stream, gateway log, the exact run plan).
  The runner tears down containers, networks, and secret files on every
  exit path. Each teardown step is best effort: a docker call that fails
  is not retried and never replaces the run's own error, so something can
  be left behind. A sweep at the end of the run removes any container or
  volume still carrying the run's label, and on Linux the next run removes
  the containers of a run whose process died.
- **`agentry install ns/name`** lays the verified release down in the
  runner home, where `agentry run` reads it — no second fetch — with the
  Claude-skill or AGENTS.md shape beside it (the policy's coding agent or
  provider decides which); a newer version replaces the older, and
  `agentry list --check` says when one exists.
- **Installed into an existing coding agent instead** (`agentry install
  --target claude-skill|agents-md`), the same contract compiles to the
  host's built-in tool grants — nothing is bundled in either mode.

Exit codes: `0` ok · `1` cli (an interrupted run, an unknown adapter, an
intake error) · `2` usage · `10` verify · `11` policy/secret ·
`12` compile · `13` gateway/docker · `14` agent · `15` timeout ·
`16` budget · `17` output schema. Task input: `--input FILE`, else piped
stdin. Runs compose: `agentry run a ... | agentry run b ...`.

Around the enforcement core (v2): **`agentry setup`** configures your LLM
provider and stands up the gateway plane before any agent is involved.
**MCP profiles** store a BriefAgent's keys and identities once per exact
release — guided setup on first run, then keep or remove on completion
(`--keep-profile`, `agentry profiles list|remove`). **`--file`** hands a
run external files read-only under `/inputs`.
**`agentry install --provider claude|codex|openclaw`** installs for a
host agent application instead of the gateway path.

The operator policy (`~/.agentry/runner/policy.yaml`, created on first
use) pins images, names the model endpoint, and gates wildcard egress.

The as-built architecture is [ARCHITECTURE.md](https://github.com/blaktron/agentry-notes/blob/main/ARCHITECTURE.md) §6
in agentry-notes, where [RUNNER-DEVIATIONS.md](https://github.com/blaktron/agentry-notes/blob/main/agentry-cli/RUNNER-DEVIATIONS.md)
records the divergences from it. The original design records
(RUNNER-PLAN, -2, -3) sit in the same repository under
[archive/agentry-cli/](https://github.com/blaktron/agentry-notes/tree/main/archive/agentry-cli).

`make accept-runner` runs the r0, r1 and r2 acceptance scripts; the docker
E2E self-skips without a daemon. Every runner acceptance script points
`AGENTRY_CONFIG_DIR` (and `AGENTRY_RUNNER_HOME`, where it needs one) at its
own throwaway directory, so the consent records `agentry run` writes never
land in your `~/.config/agentry/installed.yaml`. `AGENTRY_RUNNER_HOME`
relocates the whole runner home — the operator policy included, so a
relocated home reads and writes its own `policy.yaml`, never yours
(agentry-cli#149).

The model key a run injects is read from the environment variable the
policy names (`model.apiKeyEnv`). With `apiKeySource: keyring` in the policy
the OS keychain is the fallback when that variable is unset — the entry
`provider:<provider>` under service `agentry`, which an interactive
`agentry setup` offers to store (or `--api-key-source keyring` records the
source for a key stored elsewhere, the desktop's model step say); the
variable still wins when set, and the value is never in the policy, a plan
or a receipt (the receipt's `model.apiKey` line names the source). On Linux
the keychain is the Secret Service (GNOME Keyring, KWallet); without one the
store step fails by name and the variable is the way (agentry-cli#148).
Beside the key sits a second entry, `stored:provider:<provider>`, whose
value is only the word `stored`: the desktop's settings ask that one
whether a key is there, so answering never reads the key; `agentry
doctor` reads the key's entry itself, as a run would, and puts the marker
right (agentry-cli#156).

An Anthropic subscription token (`claude setup-token`, `sk-ant-oat…`) is a
credential of its own kind: Anthropic admits it from Claude Code alone, so
it drives `agentry run --adapter claude` and nothing else, and the container
harness refuses it by name. The policy records the kind (`model.apiKeyKind:
subscription`, written by `agentry setup` from the key's shape when it stores
one, or by `--api-key-kind subscription` for a variable you set yourself);
the claude adapter then passes it as `CLAUDE_CODE_OAUTH_TOKEN` and runs
without `--bare`, which bare mode could not read, refusing the run if a
`CLAUDE.md` sits in an ancestor of the run directory; a run whose key turns
out to be the other kind is refused before the harness starts, and `agentry
doctor` says so (agentry-cli#171, DR-26).

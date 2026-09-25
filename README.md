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

Every build that ships is released under a version (DEV-148, agentry-cli#327
and #241). A tag is one of two forms, and the branch line its commit is on
must agree:

- `vX.Y.Z` on a commit on `main`'s first-parent line (a promotion's merge
  commit) is a **production release**; its binaries bake
  `https://secureagentry.net`;
- `vX.Y.Z-dev.N` (N from 1) on a commit on `dev`'s first-parent line (a PR's
  merge commit) is a **dev prerelease** of the version `main` releases next;
  its binaries bake `https://dev.secureagentry.net`.

`build/release.env` names both registries and is the same on both branches;
`scripts/release-registry.sh` reads the line from the commit's first-parent
history (agentry-cli#240), and `scripts/release-tag.sh check` refuses a tag of
any other form, a form on the wrong line, and any 1.x unless the operator has
set the repository variable `AGENTRY_ALLOW_V1=true` (agentry#522). The
`release` workflow runs that check before it builds anything.
`agentry --version` prints the version (the tag without its `v`, `-dev.N`
included) and the registry the binary falls back to.

**Cutting one** (agents may; only the operator moves the minor version or cuts
1.0.0). Patch releases are minted as work lands:

```bash
git fetch --tags origin
tag=$(scripts/release-tag.sh next dev) && git tag "$tag" origin/dev && git push origin "$tag"    # a dev prerelease of dev's head
tag=$(scripts/release-tag.sh next main) && git tag "$tag" origin/main && git push origin "$tag"  # a production release of main's head
```

`next` names the version `main` releases next: the highest version `dev` has
prereleased above the newest release, else the newest release with its patch
bumped (the first release of all is `v0.6.0`). Releases only move forward:
`check` refuses a tag below the newest release and a prerelease of a version
already released. `RELEASE_VERSION=X.Y.0` is the operator's, for the first
tag of a minor they have chosen; an agent never sets it on its own. Tag the branch's head
only after its merge is verified: a dev prerelease after the change is
deployed and checked on dev, a production release after its promotion. Then
watch the `release` run to green and read the Release it published.

**What the workflow does.** It builds the five binaries with `make dist`
(linux and darwin on amd64 and arm64, windows on amd64), writes `BUILD-INFO`
and `SHA256SUMS`, and signs what this repository can sign: signed where
signing exists, unsigned where it does not (DEV-148), and `BUILD-INFO`
(inside the sums) and the Release's notes say which.

- The Windows exe is signed against Azure Artifact Signing in the same job
  when the five signing variables are set (agentry-cli#197). The job's
  identity is the OIDC token GitHub mints for it in the `release` environment,
  so it holds no Azure secret; `jsign` signs, and `osslsigncode` verifies to
  the Microsoft root pinned in `build/windows/signing/` and the CN
  `release.env` names. With none set it ships unsigned (an empty `signed_windows=`, the site's grammar).
- `SHA256SUMS` is signed with the estate's own update-signing key when the
  `AGENTRY_UPDATE_SIGNING_KEY` secret exists (the desktop's `signrelease
  -product agentry-cli`, at the desktop commit the workflow pins: the signed
  comment names this product, and the tool verifies what it wrote against the
  committed key set). Without it there is no `SHA256SUMS.minisig`
  (`signed_sums=none`).
- A signing input that is half-configured, or a signature that fails, fails
  the release; there is no silent fallback. Once signing is set up, the
  repository variable `AGENTRY_REQUIRE_SIGNING=true` (agentry-cli#197) makes a
  missing input fail too, so removing a credential cannot quietly turn signing
  off.

What the checks do not stop: a pushed tag runs the workflow of the tagged
commit's own tree, so they bind a tag on `main`'s or `dev`'s head (reviewed
code) and catch a mistaken tag elsewhere only while its tree carries them;
closing that needs a tag ruleset (DR-33, DR-40).

The bundle is uploaded as one workflow artifact, `agentry-cli-<version>`, and
**every tag publishes a GitHub Release** with it: a `-dev.N` tag's is a
prerelease, a `vX.Y.Z` tag's a full release, and only the newest `vX.Y.Z` is
the latest. Notes are generated from the commits under a header naming the
signatures. Before 1.0.0 every release publishes at once. From 1.0.0 on, a
minor or major release (`vX.Y.0`) is left a draft: the operator approves its
notes and publishes it. The site's downloads page does not read
it yet: until agentry-web#214 and agentry-web#444 import the Release (the dev
page taking `-dev.N` prereleases, production's the `vX.Y.Z` releases), the
site still builds the CLI itself. A `workflow_dispatch` of `release` from `main` is a dry run:
it signs exactly as a tag does, publishes no Release, and the site refuses its
bundle.

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
`setup` · `doctor` · `run` · `knowledge` · `uninstall` · `profiles` · `verify` ·
`search` · `info` · `yank` — run `agentry --help` for the full surface. `whoami` prints
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

### Knowledge packs: `knowledge create` and `knowledge add`

`agentry knowledge create NAME --file PATH…` makes a knowledge pack in
`--dir` (default `./NAME`), and `agentry knowledge add DIR --file PATH…`
extends one. The built-in **Knowledge Creator** does the work: a package
this binary embeds, run with the attestation state `builtin`, never a
release anyone installs.

1. The runner decodes the documents first, in an image holding only
   `pdftotext`, `pandoc` and `tesseract`, with no network and every
   capability dropped. PDF goes through `pdftotext`; DOCX, ODT, EPUB and
   RTF through `pandoc`; PNG, JPEG, TIFF and WebP through `tesseract`'s OCR;
   and PPTX and XLSX through the runner's own reader. Markdown, text, HTML,
   CSV, JSON, YAML and source files pass as they are.
2. The Creator compiles their text into cited pages, against the model your
   runner policy names. It runs no command itself.
3. The build applies what it wrote. The documents and their decoded text are
   kept in the pack's `sources/`.

A local build costs no credits, and nothing it reads or writes is scanned:
the command says so before it starts, and the pack records `not scanned`.
`--timeout`, `--max-turns` and `--max-cost` bound the run. A run stopped by
one keeps the pages it wrote, and an `add` with the same documents
continues. With `--json` the result is one document on stdout. Attach a
pack to a run with `agentry run <ref> --knowledge DIR`.

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
  `agentry list --check` says when one exists. Each file of the installed
  release is recorded under the runner home's integrity key, and a run
  refuses one whose files changed after the install (exit 10, naming
  them; `run --plan` says so first). `agentry install --repair ns/name@version`
  lays it down again from a fresh download, checked as any install is. A
  `--local` directory is the one exemption: its cached copy is replaced
  from the directory. The record stops an edit by anyone who cannot also
  read or replace the runner home's key; it does not stand against
  someone who can write the runner home itself.
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
run external files read-only under `/inputs`. **`--knowledge DIR`**
attaches a knowledge pack — an LLM wiki of at most 10 MB with the
documents it was compiled from — read-only at `/knowledge/<name>`, which
the run searches with `knowledge_list`, `knowledge_search` and
`knowledge_read`; the receipt names each pack by digest and says whether
it was scanned. Attaching a pack changes neither the plan nor its hash.
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

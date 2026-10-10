# agentry — the Agentry CLI

The command-line client for [Agentry](https://secureagentry.net):
publish, install, verify, and **run** QuickAgents — single-purpose AI
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
time — offline. Start a new QuickAgent from the
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
(linux and darwin on amd64 and arm64, windows on amd64), signs what this
repository can sign, then writes `BUILD-INFO` and `SHA256SUMS` over the
signed set: signed where signing exists, unsigned where it does not
(DEV-148), and `BUILD-INFO` (inside the sums) and the Release's notes say
which.

- The Windows exe is signed against Azure Artifact Signing in the same job
  when the five signing variables are set (agentry-cli#197). The job's
  identity is the OIDC token GitHub mints for it in the `release` environment,
  so it holds no Azure secret; `jsign` signs, and `osslsigncode` verifies to
  the Microsoft root pinned in `build/windows/signing/` and the CN
  `release.env` names. With none set it ships unsigned (an empty `signed_windows=`, the site's grammar).
- The two darwin binaries are signed on the Mac runner registered for this
  repository (`mac-cli-1`, beside the desktop's `mac-1`) when the `release`
  environment holds the five `APPLE_*` secrets (agentry-cli#198, DR-59).
  `scripts/sign-darwin.sh` imports the Developer ID certificate into a
  keychain it makes and deletes, signs each binary under the hardened
  runtime with Apple's timestamp and the identifier `net.secureagentry.cli`,
  checks the identity and Team ID `release.env` names, notarizes a zip of
  each and waits for `Accepted`, and asks Gatekeeper for `Notarized Developer
  ID` (`spctl --type open --context context:primary-signature`, and `codesign
  --check-notarization`; `spctl --type execute` rejects every bare binary). A bare binary cannot be stapled, so macOS checks
  the ticket online when a browser-downloaded copy first runs; a binary
  fetched with `curl` is never checked. `BUILD-INFO` says
  `signed_darwin=developer-id TEAM=<id> notarized=true`, or nothing when the
  secrets are not set.
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

**The built-in packages.** Every tag also publishes the packages the binary
embeds (the Knowledge Creator, the Drafter, the Voice Writer and the
Examiner) to its channel's registry. The `builtins` job runs the release's
own Linux binary as
`agentry builtins publish` ([below](#the-built-in-packages-builtins)). It
reads the PAT of an account with publish rights in the registry's `agentry`
organization from the `release` environment's secret
`AGENTRY_BUILTINS_TOKEN_DEV` for a `-dev.N` tag
or `AGENTRY_BUILTINS_TOKEN_PRODUCTION` for a `vX.Y.Z` tag, and only the
channel's own token reaches the step. With the secret
missing, the job fails and the release's built-ins are not in the registry.

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
`scripts/check-windows-binary.sh`, `scripts/sign-darwin.sh`) are the release
workflow's, and `scripts/acceptance/windows-signing.sh` proves the Windows
seam on every PR with a throwaway CA in place of the key. The macOS seam has
no PR check (it needs a Mac and Apple's notary service); a `workflow_dispatch`
dry run proves it.

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

`init` · `validate` · `login` · `logout` · `whoami` · `publish` · `install` · `list` ·
`setup` · `doctor` · `files` · `run` · `schedule` · `knowledge` · `voice` · `report-design` · `draft` · `review` · `builtins` · `uninstall` · `profiles` · `connect` · `connections` · `model-signins` · `verify` ·
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
(`run` takes `--output-format json`; a run that fails says where in its
envelope's `stage` and, when the step that stopped it decided why, in its
`reason`: a code from a closed list, such as `exec-not-provided` or
`mcp-install-failed`, with the release's own declared names it concerns
and a detail word, never text or a path, so a host that keeps nothing else
can still say why; agentry-cli#628, `receipt.ReasonCodes`), and `install --plan --json` /
`run --plan --json` print the consent-free preview a client shows a person
first. `files ensure` pulls the files image the runner policy names (the one
the decode step turns a run's documents into text in), by digest, and prints
its reference, then refreshes the ClamAV signatures every input is checked
against, so no run has to download either; a sandbox host runs it at each
update. Server-side end-to-end coverage
lives in the main repo's `scripts/acceptance/m3-cli.sh`, which builds this
CLI against a live registry.

### Machine sign-in and local sign-out

`agentry login --json --registry https://registry.example` starts browser
sign-in even with no terminal. Stdout is **NDJSON**, not a single document:
one `format: 1`, `status: pending` event carrying `registry`, `authorizeUrl`
and `expiresInSeconds`, then one final event. Keep the CLI process alive and
parse each line. The authorization URL carries this flow's state nonce;
neither the callback code nor token is returned. The browser must reach
this process's `127.0.0.1` listener. `AGENTRY_NO_BROWSER=1` lets the host open
the returned URL itself. This is a local browser flow, not a device-code grant.

Final statuses are `signed_in`, `declined`, `cancelled`, `timed_out` and
`failed`. Success carries the verified `handle`, token `storage` (`keyring`
or `file`), and `signingKeys` (`pinned`, `unchanged`, `unsigned`, `rotation`,
`mismatch` or `unavailable`). Rotation/mismatch never silently re-pin.
Other outcomes carry a fixed `reason` and exit nonzero. Raw registry,
transport and storage errors are withheld because they can echo secrets.
Normal terminal login, PAT paste, signing-key pinning and Knowledge rewrap
remain available. JSON login refuses `--paste` and an active `AGENTRY_TOKEN`
override; unset that override outside the host's chat first, so a later
`whoami --json` inspects the account that was just signed in.

Send one JSON line on stdin: `{"action":"cancel"}` for cancellation or
`{"action":"decline"}` when the person declines in the host. SIGINT/SIGTERM
also cancel; EOF alone does not. Unknown/oversized input is `failed` with
`invalid_control`, without echoing it. `--timeout` sets a positive duration
up to five minutes for JSON sign-in. A nonce-bound `access_denied` callback
also reports decline. The current registry browser Cancel link simply leaves
the authorization page; it does not send that callback. A host must send
its decline/cancel control or signal the process, otherwise it waits for expiry.

`agentry logout --json --registry https://registry.example` removes that
URL's local login and returns `format: 1`, `status: signed_out`. It does not
revoke the PAT at the registry or remove trust pins, default registry,
installed packages, model keys, consent records or other registry identities.
A credential tombstone prevents the old host-keyed keyring fallback from
silently signing that URL back in. Sign-out refuses `shared_keyring_identity`
when a recorded keyring entry shares its host with another recorded keyring
identity; it changes neither entry. This existing host-keyed aliasing needs
the migration tracked in agentry-cli#267.

An environment token cannot be unset by a child process: when present,
logout reports `environmentCredential: true` and removes local storage only.
Unset the override separately; it continues to authenticate CLI commands.
Sign-out is idempotent. Machine auth accepts HTTPS registry URLs and loopback
HTTP for local registries, without embedded credentials, query or fragment.
`whoami --json` remains the account inspection operation.

### Knowledge packs: `knowledge create` and `knowledge add`

`agentry knowledge create NAME --file PATH…` makes a knowledge pack and
keeps it sealed on this computer (below), and `agentry knowledge add NAME
--file PATH…` extends one. `--dir DIR` makes a plain pack directory instead,
in the clear, and `add DIR` extends one. The built-in **Knowledge Creator**
does the work: a package
this binary embeds, run with the attestation state `builtin`, never a
release anyone installs.

1. The runner checks each document with ClamAV, then detects it by content
   and decodes it before the agent, in the signed, digest-pinned files image
   with no network and every capability dropped. A document ClamAV finds
   refuses the build, naming it, and so does a risky one Hybrid Analysis
   calls malicious (looked up by its SHA-256 alone, as for a run). PDF uses `pdftotext`; DOCX, ODT, EPUB and RTF
   use `pandoc` to GitHub Markdown; images use `tesseract`'s English OCR.
   XLSX and ODS become capped Markdown tables; PPTX and ODP become a
   section per slide. Legacy Office, mail and other formats use
   Apache Tika. Any other UTF-8 text passes as it is, byte for byte: code,
   markup (HTML, XML, SVG), configuration and data (CSV, JSON, YAML),
   told by `file` without starting Tika (agentry-cli#630); a file no reader can
   decode is listed as unread with its reason. An archive is listed as
   unread too: a pack keeps each document as given, and a build does not
   take what an archive holds yet (agentry-cli#619).
2. The Creator compiles their text into cited pages, against the model your
   runner policy names. It runs no command itself.
3. The build applies what it wrote. The documents and their decoded text are
   kept in the pack's `sources/`.

A build takes documents, a brief, or both (agentry-cli#523). `--brief TEXT`
(or `--brief-file F`) says what the pack should cover and for whom, at most
8 KiB, and is kept as `sources/brief.md` (`brief-2.md` and on, beside an
earlier one); `create NAME --brief "…"` with no `--file` makes a pack from
research alone. Specific documents make the best knowledge: without them
the Creator researches the web and its own knowledge, which can be wrong or
out of date, and the command warns so. `--web` lets the Creator search,
on by default for a new pack with no documents and off otherwise
(`--no-web`; an `add` extends documents the pack holds, which searches would
carry to the search provider): it is given `web_search` and `web_read`, the
Drafter's web tools without `registry_search`, AI Review reads every result
before its model does, and one flagged or not scanned is withheld. Because
the Creator reads documents no one scanned, its `web_read` opens only an
address one of its searches returned, so an instruction hidden in a document
cannot send another document's text out in an address of its own. On this computer that takes your
own TypeSafe key and a search key, as `draft` does; without either the build
runs without the web and says why. Its instruction has it search only for
what the documents and the pack do not hold. A page compiled from the web
cites `[src: web:<url>]` and the pack keeps no copy of it; what the model
knows itself is cited `[src: model]`. Searching can raise what a build
costs. `--hosted` is a sandbox host's build, whose searches and scans go
through the registry with the job's credential, as a hosted draft's do.

A local build costs no credits, and nothing it reads or writes is scanned:
the command says so before it starts, and the pack records `not scanned`.
`--timeout`, `--max-turns` and `--max-cost` bound the run. A build has 60
minutes, or your runner policy's run timeout when that is longer; `--timeout`
sets it for one build, shorter or longer. It has no token limit, and its
tool-call cap is the policy's. A run stopped at one of these bounds keeps
the pages it wrote, and an `add` with the same documents continues. With
`--json` the result is one document on stdout. With
`--output-format json`, as `run` takes it, stdout is instead the Creator
run's result envelope (its usage split into input and output tokens, the
attestation `builtin`, `capabilities_enforced`) with that document under
`knowledge`, and the harness's usage is narrated live on stderr
(`usage: input=… output=… total=… turns=…`); a build that fails before the
Creator runs still writes one envelope, with its class and stage. It is the
form a sandbox host drives for a build made in Agentry, which settles on
the envelope and stops a build at its hold from the narration. Attach a
pack to a run with `agentry run <ref> --knowledge NAME` (or `DIR`).

### Keeping packs: the local store, and Agentry

Packs are kept in the store under the runner home, `~/.agentry/runner/
knowledge/<pack id>/`, which Agentry Desktop shares: `manifest.json` in the
clear, the tree as one sealed file (`pack.sealed`, AES-256-GCM frames under
a random data key per pack, the core's `pkg/packseal` format), and
`key.json`, that data key as it is wrapped.

- **Signed in**, the registry wraps the key for your account (`POST
  /knowledge/keys/wrap`); Agentry holds the key that opens it. The pack then
  opens on any computer you sign in on, with nothing carried between them,
  and the unwrapped key is cached in the OS keychain (`knowledge:<pack id>`)
  so it opens offline afterwards. A token minted before the registry had the
  knowledge scopes is refused: `agentry login` again.
- **Signed out**, this computer's key wraps it (`knowledge:machine` in the
  keychain), which is not portable. The next signed-in command, or
  `agentry login`, rewraps it to your account. With `AGENTRY_NO_KEYRING` and
  no sign-in, nothing can be kept sealed, and the command says so.

| Command | What it does |
|---|---|
| `knowledge list` | the packs kept here: pages, size, scan record, where they are kept, what their key is wrapped to |
| `knowledge list --org ORG` | the packs your organization ORG shares in Agentry, and whether its members may export each |
| `knowledge import DIR` | keep a pack directory sealed here (DIR is left as it is) |
| `knowledge upload NAME` | send it to the registry, sealed, where it is scanned and counted against your allocation |
| `knowledge download NAME` | keep Agentry's copy here, checked in full |
| `knowledge download --org ORG NAME` | keep ORG's shared pack here: its key stays the organization's, asked for at every open and never kept in the keychain, so it opens only while you are a member and signed in |
| `knowledge export NAME --to DIR` | write it out unencrypted into a new or empty DIR, after a typed `yes` (`--yes` for scripts); a shared pack is exported by Agentry, for the organization's admins, and for its members once an admin marks it exportable |
| `knowledge delete NAME` | delete it and its cached key; `--remote` also deletes Agentry's copy. The only copy is deleted after a typed `yes` (`--yes`) |

`run --knowledge NAME` opens a kept pack into a 0700 directory inside the
store for the run to copy, and removes it at teardown; the run's copy goes
at teardown too, even with `--keep-run-dir`. A bare name is looked up in
the store first, and a directory of that name in the working directory is
taken only when the store keeps no such pack. A pack that is not kept, or
will not open (signed out of the registry that wraps its key, another
account's, altered), is refused before the run with exit 11, naming the
pack. A kept `create` or `add` works on the pack in the same kind of
directory, and removes it with the Creator run's output, which holds the
pages it wrote, unless `--out DIR` asks to keep that output.

### Your Writing Voice: `voice`

A **Writing Voice** is one file, `VOICE.md`, of instructions an LLM follows
to write the way you do (plan `writing-voice.md`). `agentry voice create
--file PATH…` runs the built-in **Voice Writer** on samples of your writing
(decoded as the Knowledge Creator's documents are, 10 MB in all) and keeps
the voice it writes on this computer; `improve --file PATH…` runs it again
over the voice and more samples, `edit --note TEXT [--file PATH…]` changes
the voice by a note (an AI Edit), with more samples that show the change if
you give them, and `set FILE|-` keeps your own text. `--brief TEXT` says how
you want to sound (create and improve); an edit's note is the change to
make. Like a knowledge build, it runs against the model your runner policy
names, costs no credits, and nothing it reads or writes is reviewed.

The voice is kept at `<runner home>/voice/`, sealed the way a kept pack is
(`voice.sealed`, `key.json`, and `voice.json`, its record in the clear: the
digest, the size, where it came from, whether it attaches to every run). It
is at most 32 KiB, and it never rests on disk in the clear.

Every `agentry run` attaches it while it is set to attach (the first voice
is, and says so): `voice attach off` stops that, `--no-voice` skips it for
one run and `--voice` attaches it for one run when it is off. A run reads it
through one tool, `voice_read`, and the System Prompt says it governs how the
run writes and nothing else, and that a format the task fixes wins.
Attaching it changes neither the run plan nor its hash; the receipt names it
and its digest. A built-in's run never takes it.

| Command | Does |
| --- | --- |
| `voice show` | print the voice (`--json`: its record) |
| `voice attach on\|off` | attach it to every run here, or only with `--voice`; with `--account`, set your account's flag for runs In Agentry |
| `voice delete` | delete it and its cached key |
| `voice upload` | submit this computer's voice to your account; AI Review reads it there, and once it passes it replaces the account's voice (`--wait` waits for the verdict; a flag exits non-zero with its findings). This computer's voice does not change |
| `voice download` | keep your account's live voice on this computer, asking before it replaces a different one (`--yes`) |
| `voice status` | this computer's voice and your account's: the live one, a pending one in review or flagged with its findings, the previous one (`--json`) |

Your account keeps a voice of its own, for runs In Agentry, and the two are
never synced on their own (plan D6). The text goes to the registry in the
clear over TLS and is sealed there, because the registry reads it to review
it; a downloaded voice is held to the digest the registry names before it is
kept, and the receipt of a run that attaches it says it was reviewed in the
account and not checked again here.

For a driver such as a sandbox host: `create`, `improve` and `edit` take
`--current FILE` (the voice as it stands, instead of the one kept here),
`--write FILE` (write the result there, keep nothing) and `--max-text-bytes
N` (cap the samples' decoded text), with `--output-format json` as `knowledge
create` has; `run --voice-file FILE` attaches a `VOICE.md` it names.

### Shipping a pack with a release

A QuickAgent can carry packs of its own. Put each one, as `knowledge
create --dir` or `add` keeps it, under `knowledge/<name>/` in the package,
name it in the manifest's `knowledge.ships`, and include `knowledge/**` in
`publish.include`. The registry refuses a release whose pack is not the one
its `manifest.json` describes, so change a shipped pack with `agentry
knowledge add`, never by hand. The registry scans each shipped pack as part
of the version's scan and signs it with the rest of the archive.

- **Publishing.** `agentry publish` refuses, before uploading, a release
  whose `publish.include` leaves a shipped pack out. It also refuses an
  upload past 16 MiB compressed, the most the registry's upload takes;
  publish a larger release from its tag instead.
- **Installing.** The consent lists `knowledge: ships <name> (<size>)` with
  the other capabilities, and `installed.yaml` records the names. An update
  that ships one more pack asks again.
- **Running.** A run mounts each shipped pack read-only at
  `/knowledge/<name>` beside any pack you attach with `--knowledge`, from
  the release's own verified files, never fetched again. A pack you attach
  cannot take a shipped pack's name. The receipt names a shipped pack as the
  release's: `<name> shipped with the release (archive sha256:…)`.

### Drafting a QuickAgent: `draft`

`agentry draft "<description>" [--hint TEXT…] [--knowledge PACK…] [--out DIR]` drafts a
QuickAgent from a plain description of what it should do
(`--description-file FILE` reads it from a file). The built-in
**Drafter** does the work: a second package this binary embeds, run with the
attestation state `builtin` like the Knowledge Creator. It searches the
registry for a published QuickAgent that already does the task, researches
how the task is done, and writes a draft. It never submits anything.

**AI Review reads what the Drafter reads, first.** TypeSafe's Jev reads the
description and the hints before the Drafter starts, and a description it
flags, or cannot scan, stops the draft (exit 11) before anything is searched
or read. While the Drafter works, AI Review reads every search result, page
and registry answer before the Drafter's model does. What it flags, or
cannot scan, is withheld: the model gets the address it asked for and the
reason, never the text, and the draft's notes list it. A page that cannot be
read at all is answered in the tools' own words, never the server's. On this computer the scans use **your own Jev
key**, and without one the Drafter does not run. The key comes from
`$TYPESAFE_API_KEY`, or the variable the policy's `judge.apiKeyEnv` names.
With `judge.apiKeySource: keyring` it comes from the OS keychain entry
`provider:typesafe`.

**The web tools** are the Drafter's alone; the compiler refuses them to any
release. They run in the tools container; the agent container holds no Jev
key, no search key and no registry credential (only the model key, as in
every run). On Linux the tools container is on a network of the run's own,
which the gateway's docker filter lets no other container join.
- `web_read` makes GET requests only, over http and https on ports 80 and
  443, to public addresses only. The address is checked at every connection
  and redirect hop, so a name that resolves somewhere private is refused
  too. It sends no cookie and no credential, and returns at most 32 KiB of
  a page's text.
- `registry_search` reads the registry's search, a QuickAgent's manifest,
  and its vulnerability check of an exact MCP server version.
- `web_search` needs a search provider of your own in the policy, each
  with its own key: `webSearch: {provider: brave}` (Brave's LLM Context API,
  `$BRAVE_API_KEY`), `tavily` (`$TAVILY_API_KEY`), `exa` (`$EXA_API_KEY`),
  `perplexity` (Perplexity's Search API, `$PERPLEXITY_API_KEY`) or `youcom`
  (You.com, `$YDC_API_KEY`); or the variable `webSearch.apiKeyEnv` names,
  or, with `webSearch.apiKeySource: keyring`, the keychain entry
  `provider:<the provider>`, such as `provider:brave`. Your searches go to
  that provider under its own terms: Tavily's and Exa's keep a right to use
  queries, and Perplexity's no-training statement covers its chat
  completions, not its search (agentry DEV-185). Without one the Drafter reads the registry and
  the pages it already knows.
- Under a sandboxed runtime (`docker.runtime: runsc`) the tools container
  cannot reach docker's own DNS on its network, so name your host's upstream
  name servers in the policy: `docker: {runtime: runsc, resolvers:
  [192.168.122.1]}`, at most three addresses, never a loopback stub such as
  `127.0.0.53`. The runner writes them into a `resolv.conf` of the run's own
  that the tools container reads in place of docker's, and the receipt's
  `web.resolvers` line names them. Without them the Drafter does not run
  under a runtime (exit 11), and `agentry doctor` says so. On the daemon's
  default runtime nothing needs setting.

The description, the searches and the pages go to the search provider, to
TypeSafe and to the sites read, so the description should hold no secret.
The draft runs against the model your runner policy names, for no credits.

**Your knowledge** (agentry-cli#522). `--knowledge PACK` (repeatable) gives
the Drafter your knowledge packs: a pack directory, or the name of a pack
kept on this computer, as `agentry run --knowledge` takes them, at most five
packs and 10 MB in all, refused before anything is scanned. The Drafter reads
them before the web, with the knowledge tools every run with a pack has: it
learns the task's domain from them and writes what the QuickAgent needs into
its instruction, so a draft with good packs makes fewer searches; it lists
the pages it used in `sources` as `pack:<name>/<page>`; and when the
QuickAgent needs your pack at run time it declares `knowledge.accepts`, with
Steps that start from it. A draft never ships a pack: a `knowledge.ships`
the Drafter writes is removed and named in the notes. `draft.json` records
each pack under `knowledge`, with how it was scanned and what was withheld.

Nothing of a pack reaches the Drafter's model unscanned:
- A pack kept here that you uploaded to Agentry or downloaded from it is not
  scanned again. Its scan in Agentry stands in, once the CLI has confirmed
  it with that registry: the copy there is ready, holds this pack's tree,
  and its scan finished every stage with AI Review reading every chunk. What
  that scan flagged, and any file it could not read as text, is withheld.
- Any other pack (a directory, a pack kept here only, one changed here since
  its upload, or one you are signed out of) is **scanned first**. The CLI
  says so before anything is scanned, then AI Review reads every file of it
  with your Jev key, as model input, before the draft starts, in the pack
  scan's chunks with each file's path. A chunk that does not pass is read
  again a file at a time, so only the file it flags is withheld. A large pack
  takes a while. A file it flags, cannot scan, or that is not text is
  withheld.

What is withheld is cut from a private copy of the pack, and its index is
cut of the lines that linked a withheld page (a withheld index is replaced
by a fixed line, never written again), so the Drafter's knowledge tools never
serve it. What the Drafter reads in a pack can shape its searches and the
pages it reads, which go to the search provider and the sites, as the
description does. The notes' "Withheld by AI Review" names each file, with
why, never its text. With `--hosted`, `--knowledge` takes only the
directories a sandbox host laid down from Agentry's store, whose manifests
carry Agentry's record.

The result, in `DIR` (default `./agentry-out/agentry-drafter-<runid>`):
- `draft.json` holds the manifest, the instructions, the notes, the
  published QuickAgents that match (`matches`), the sources, the open
  questions, AI Review's record (what was read and every withheld source
  with its reason), and `validation`, the report `agentry validate --json`
  gives on the rendered package.
- `package/` holds that package: `agent.yaml`, `AGENT.md`, a README and a
  LICENSE placeholder.

The Drafter decides rather than asks: it picks a default for what the
description leaves open and names it in its notes, asks at most three intake
questions (most drafts none or one), lists at most five things to prepare,
leaves at most two open questions, and keeps its notes to five bullets. The
instruction it writes has six parts: Role, Inputs, Steps, Output, Background
and Checks. Most QuickAgents run with no network and no web search, so the
instruction carries the domain knowledge the run cannot look up: a few
kilobytes for a small task, 20 to 30 KB for a big one, and at most 32 KiB,
the instruction bound of AI Review's check of a finished draft. A longer one
goes to the repair turn to be shortened. The check also holds the
description, the manifest and the instruction to 48 KiB together, and a
draft past that is recorded as not checked, never as passed.

`agentry draft --revise DIR` (a release's package folder) or `--revise-ref
ns/name@version` (a published release, fetched with the command's credential
and held to the digest the registry resolves) drafts that release's next
version instead of a new QuickAgent. The Drafter is given the release's
`agent.yaml` and instruction file as files under `/inputs/revise`, never the
rest of the package, and the task holds what you want changed
(`--instructions-file`) and the problems you found (`--problems-file`), each
at most 4,000 bytes and at least one given. AI Review reads those two texts,
then the release's two files, before the Drafter starts; a text or a file it
does not pass stops the revision there. The revision keeps the
release's name (a draft that names another is corrected, and the notes say
so), its notes say what changed and why, and `draft.json` records the
release under `revises`, its ref and its archive's digest. A revision takes
no description, hints or knowledge packs. A driver can fetch `--revise-ref`
from a base of its own with `--revise-registry URL`, which must be on
`--registry`'s origin. A sandbox host names the runs extension's release
prefix there and opens it with the run's grant in `AGENTRY_RUN_GRANT`. The
draft's scans keep `--registry`.

Before a draft is written, the CLI checks it in four steps:
1. **Mechanical slips are corrected without the model.** A mode, a tag or a
   choice named twice loses the second one. An optional part left empty, or
   holding a placeholder such as "None", is removed. A list past its limit
   is cut to the limit, and a text past its limit is shortened at a word,
   ending with "…". The limits are the rules' own. The notes name each
   correction under "Corrected after drafting", and nothing is invented.
2. **The rules check it**, with `agentry validate`'s own code.
3. **AI Review checks the finished draft** (agentry-cli#418). It reads the
   rendered package, exactly the files `publish.include` takes, the licence
   among them, two ways: with the release rubric, asked as the pipeline's
   stage S5 asks it at publish, and with one question on whether the
   instructions do the task you described (`off_task`). On this computer it
   uses your own Jev key; `--hosted`, the registry's
   `POST /api/v1/model-input/check` with the job's credential, which the
   registry admits at most twice a draft and which the CLI never retries.
   The verdicts go into `draft.json` under `aiReview.check`, and the notes
   say what a flag means under "AI Review's check". The release rubric's
   findings on a draft are advisory (agentry-cli#633): each is recorded
   with the severity stage S5 would give it, and named in the notes with
   what it does at publish (a high finding sends the version to a person,
   a medium one is shown on the report of a version that publishes), and
   none is sent to the repair turn. A description over 8 KiB
   is not checked, and neither is a draft that could not be rendered. A
   pass does not mean the draft does all of the task, and the pipeline
   scans the whole version at publish whatever the check said.
4. **One repair turn.** If the rules still refuse it, its instruction is
   past 32 KiB, or the check reads it as not doing the task (`off_task`),
   the Drafter runs once more, without its web tools. Its task is the
   rules' report, what the check flagged, and its own manifest and
   instructions, and nothing else: no notes, no page. With a flag it is also
   given the task the first turn was given, which AI Review passed before
   that turn started. The second answer is checked again, so a draft asks
   at most two checks. The turn is told to change only what the report
   names, and a second answer is not kept when it breaks rules the first
   passed, or when it no longer declares an MCP server, an egress host, a
   command or a secret of the first's that the report did not name, even
   if it mends what the first broke: a repair never clears a report by
   stripping what the task needs. If the runner refused a last message for
   prose before its JSON object, the post-step recovers the object only when
   it ends the message (apart from a closing fence) and passes the Drafter's
   output schema. The draft records `recovered` and its notes say how much
   text was dropped; the original message stays in the receipts. Recovery
   leaves the repair turn available for a rules failure. An answer that
   cannot be recovered (exit 17) gets the same turn, given the error and that
   message back. It returns the manifest again, and the instruction file too if
   the report names it. The second answer is corrected and checked like the
   first, and kept whether or not it passes; its notes say "Repaired once"
   and what the rules said of the first. A repair turn that returns no draft
   leaves the first, with its problems and a note saying why, or, when the
   first was not a draft either, ends with exit 17 and says why the repair
   returned no draft. The turn's
   budget keeps at most 8 tool calls and what is left of `--max-cost`.
   When the draft has a timeout, the repair gets at most 300 seconds within
   the time left. A draft with no timeout gives its repair turn none either.
   Its receipts are in `DIR/repair/`.

A draft that still fails the rules is kept with its problems for you to fix.
Review every part before you publish it. `--timeout`, `--max-turns` and
`--max-cost` bound the run. On this computer a draft has 30 minutes, or your
runner policy's run timeout when that is longer; `--timeout` sets it for one
draft, shorter or longer. It has no token limit, and its tool-call cap is
the policy's. A hosted draft runs under the caps its job carries. With
`--output-format json`, as `run` takes it,
stdout is the Drafter run's result envelope (its usage, the attestation
`builtin`, the `web` counts) with the draft's report under `draft`: where
`draft.json` is, whether the package was rendered and passed, how many
matches, open questions and withheld sources it has, how many slips were
corrected (`corrections`), whether it was repaired (`repaired`) or recovered
from a refused message (`recovered`, when true), and AI
Review's check as one word (`check`: `pass`, `flagged`, `not_scanned`, or
`unanswered` when it has no answer). The
envelope's usage is both turns' together, and the usage lines the repair
turn narrates count from the first turn's total. The envelope's `result` is empty,
because the draft is in `draft.json`. The harness's usage is narrated live
on stderr (`usage: input=… output=… total=… turns=…`). A draft that stops before the
Drafter runs still writes one envelope with its class and stage; a
description AI Review did not pass is exit 11 at stage `description`.
`agentry draft --hosted --output-format json` is what a sandbox host runs
for a draft made in Agentry, from the site's or the desktop's Describe it:
the scans and the search go through the registry, with the job's credential
in `$AGENTRY_JOB_TOKEN`, and no Jev or search key is on the host. The draft
kind, its hold and its settlement are in the core's
[hosted runs](https://github.com/blaktron/agentry/blob/main/docs/hosted-runs.md#a-hosted-draft)
page.

### Reviewing a QuickAgent before Submit: `review`

`agentry review [dir]` runs the built-in Examiner over the QuickAgent in
`dir` (default `.`) and reports what Agentry's pipeline would be expected to
say about it if you submitted it (plan
`desktop-review-key-and-run-cards.md` §6; agentry-cli#504). It runs on this
computer against the model your runner policy names, with your own TypeSafe
Jev key for AI Review (`$TYPESAFE_API_KEY`, or the policy's `judge` block),
for no credits, and submits nothing. Without a key it refuses (exit 11) and
says where to add one.

1. **Before the run**, the registry's own rules run here, as `agentry
   validate` runs them: the manifest rules, the inventory checks, and the
   hidden-content and injection rules under the registry's current rule
   bundle (fetched first, as `validate --rules-update` does; the cached or
   embedded bundle when the registry does not answer, and the report names
   which). The registry is asked for its baseline publication policy
   (`GET /platform/policy`) and for its record of each declared MCP server
   (`GET /mcp-servers/scan`). The files a submission would carry are then
   copied, with the manifest's canonical JSON, into the run's inputs.
2. **The run** reads that copy read-only. Nothing of the QuickAgent runs.
   The Examiner asks `ai_review` (the review grant above), and its model
   reads every file, each read by AI Review first, for secrets, for scripts
   that reach past the declared capabilities, and for text aimed at the
   review. The task it is given carries none of the package's text.
3. **After the run**, `DIR/report.json` is written beside the run's
   receipts (`--out DIR`, default `./agentry-out/agentry-examiner-<runid>`;
   a folder that already holds a report or a run's receipts is refused),
   printed, and the command exits by the expected outcome: `0` published,
   `20` held for review, `21` rejected, `22` refused at Submit (the
   manifest rules refuse it), `23` not determined here. A run that failed
   writes the report as far as it got and exits with its own class, except
   one the runner refused only at the output schema whose reading was
   recovered (below): its receipt still says it failed, the report marks
   the reading `recovered`, stderr says so, and the command exits by the
   outcome.
   `--json` prints the report itself.

**The report** (`schemaVersion` `"1"`; `pkg/examiner.Report`):

- `expected`: the `outcome` (`published`, `needs_review`, `rejected`,
  `refused_at_submit`, `undetermined`), the worst severity and the gate's
  rule that decides it, a `reason`, the `policy` it was decided under with
  its `source` (`registry`, or `compiled` with the `error` that kept the
  registry's out), which kinds were `counted`, and what it `assumes` (a
  public version, as the manifest rules check it; no review queue named).
- `registryRules` (kind 1): the manifest rules' errors and warnings, the
  findings (scanner id, severity, confidence, title, file and line), the
  bundle, and each declared server's record.
- `aiReview` (kind 2): whether it ran, the model, the rubric hash, the six
  questions' scores with the file that drew each, and the findings at score
  2 or more as stage S5 rates them (2 medium and 3 high; an attempted
  injection or an undeclared capability high from 2). It is read from the
  run's own record (`receipts/review.ndjson`), never from what the model
  said, and only for a run whose receipt counted a review: the first
  answered call over the staged package counts.
- `examinerReading` (kind 3): the model's summary and findings (`secret`,
  `script`, `instruction`, `other`), the files it did not read, and the files
  AI Review withheld from it. It is never counted (`counted: false`). When
  the runner refused the model's last message at the output schema (a
  sentence before the object, say), its terminal object is taken if the
  schema passes it, with only whitespace or one standalone closing fence
  after it; a fence before it is optional. The reading is marked `recovered`.
- `notRunHere`: the registry's secrets and scripts stages, the publisher
  signals, the diff against the last published version, an organization's
  policy, a shipped pack's secrets scan, URL reputation and AI Review (its
  rules run here, as the registry's stage for packs runs them), the text
  recovered from image metadata, Submit's own refusals, and whatever did
  not answer here.
- `notice`: the registry scans again at Submit, AI Review can answer
  differently on the same files, and the registry's answer is the one that
  counts.

The expected outcome is computed from the first two kinds alone, by the
gate's rule restated in `pkg/examiner` (`Gate`). The gate's code is the
core's, so `TestTheGateRestatementMatchesTheCoresTable` runs every case of
the table the core publishes (`pkg/gatecases`, agentry#776) against the
restatement, and a core pin that changes the gate fails until it follows.
Where something the gate weighs did not answer here (AI Review, or AI
Review for part of a package past one call's 4 MiB or 500 files, or a
declared server's record, which the registry looks up again at Submit once
it is a day old and which is taken as current here for 23 hours), the
outcome is named only when its best and worst answers would
decide alike, and is `undetermined` otherwise. A rule hit in a text file
past 1 MiB is taken from the file's first 1 MiB, all the registry reads.

### The built-in packages: `builtins`

The Knowledge Creator (`agentry/knowledge-creator`, behind `knowledge create`
and `add`), the Drafter (`agentry/drafter`, behind `draft`), the Voice Writer
(`agentry/voice-writer`, behind `voice`) and the Examiner (`agentry/examiner`,
behind `review`) are QuickAgent packages built into this binary, and run as
built-ins: attestation state `builtin`, no registry resolve, and grants no
other package gets (`web_search` and `web_read` for the Drafter and the
Knowledge Creator, `registry_search` for the Drafter alone; the Examiner's
review grant). Every one of them is also published in the registry,
public, from the same bytes, so anyone can read the instruction that runs
(agentry-cli#470). `agentry builtins` (or `--json builtins`) lists each with the
digest of its release archive. The registry built that archive from these
bytes, so the digest on the package's page in the directory is the same.
Installing a published built-in runs it as an ordinary release with only
what its manifest declares. A built-in runs as `builtin` only from the copy
inside the binary.

**The review grant** is the second grant a built-in can be given, for the
Examiner, which reviews a QuickAgent you built before you submit it
(`review`, agentry-cli#504), and for it alone. Like the web
tools it rides on the built-in itself: the compiler refuses it to any other
package, beside the web tools, and to a built-in that declares egress, exec,
a filesystem write, a secret or an MCP server. With it:
- `ai_review` asks AI Review's release rubric, the six questions the
  registry's stage S5 asks, about the package in the run's inputs (its
  manifest as canonical JSON, staged by the client, and every text file),
  and answers with each question's score and the file that drew it, the
  model and the rubric hash, never the package's text.
- `read_input` gives the model a file only after AI Review's model-input
  questions pass it. A file it flags, or cannot scan, is withheld: the model
  gets its path and the reason.
- Your own TypeSafe key reaches the tools container alone, and without it
  the run does not start. The tools container reaches `api.typesafe.ai:443`
  and nothing else, through the gateway's proxy; on Linux the run's docker
  filter holds the proxy to that host as well. Every file scanned and every
  review is in `receipts/review.ndjson`, counted in the receipt.

The CLI's tag release publishes them: its `builtins` job runs the release's
own Linux binary as `agentry builtins publish` against the channel's registry.
The job creates the agent record when it is missing and marks it built-in
(the registry's `builtin`, which only platform staff set and the directory
labels Built-in in place of the run modes, agentry-cli#474). It skips a
version the registry already has with the same digest, and fails on one it
has with a different digest. The built-ins live in the `agentry` organization's
namespace, so only an account with publish rights there can publish them. The
job reads that account's PAT from `AGENTRY_BUILTINS_TOKEN_DEV` or
`AGENTRY_BUILTINS_TOKEN_PRODUCTION`, and the registry refuses anyone else.

A published version is never rebuilt, so **a change to a built-in's tree
takes a new version**. Bump its `agent.yaml` and its package's `Version`
constant, and append the new line `agentry builtins` prints to
`pkg/runner/localrun/builtins.lock`. `TestTheBuiltinsLockHoldsEveryBuiltin`
fails until you do, and CI's `scripts/builtins-lock-check.sh` refuses an
edited line.

### Runs for later and on repeat: `schedule`

`agentry schedule add` gives a run on this computer a schedule, like a
calendar entry: `--once "YYYY-MM-DD HH:MM"`, `--daily HH:MM`, `--weekdays
HH:MM`, `--weekly tue,thu --at HH:MM` or `--monthly N --at HH:MM` (a month
without day N runs on its last day), in `--zone` (an IANA name; this
computer's by default), ending `--until DATE` or after `--times N` runs.
`--out DIR` is required: each run gets its own folder in it. The run
setup's flags are the run's (`--task TEXT` or `--input FILE`, `--file`,
`--knowledge`, `--intake`, `--voice`, the limits). No sign-in is needed for
a public release (plan `quickagent-scheduling.md`, DR-83).

```bash
agentry schedule add acme/digest --daily 09:00 --task "Today's summary" --out ~/reports
agentry schedule list            # id, name, version, repeat, next, last outcome
agentry schedule show <id>       # the next five occurrences and the last ten runs
agentry schedule run <id>        # Run now: an ordinary run, no occurrence used
agentry schedule edit <id> --weekly tue --at 22:00
agentry schedule pause <id>      # resume <id> skips what fell due meanwhile
agentry schedule remove <id>     # the runs' folders are kept
```

What a schedule runs is fixed when it is made. `add` resolves the release
and pins its exact version and digest (a `--local` directory too: an edited
directory refuses with exit 10). It shows the capabilities and takes your
consent on the terminal, or `--yes`. Then it runs the install step
`run --prepare` makes. A newer version needs a new schedule. Only a release
that can run autonomous can be scheduled. An `--allow-*` flag is kept only
when it lifted a refusal at add.

A scheduled run does not see your shell, so `add` refuses a schedule that
could not reach what it needs, and names each gap:
- the model key must be in the keychain (`agentry setup`);
- each declared secret must be in the QuickAgent's MCP profile or the
  policy's `envFile` source;
- a secret intake question cannot be answered by flag;
- Docker and the keychain must answer from the system scheduler itself.

`add` checks the last one by running `agentry schedule probe` there. A
systemd user manager started before you joined the `docker` group cannot
reach the daemon until you sign out of every session and back in.

`add --check` decides all of this and keeps nothing: it asks no consent,
installs nothing, seals nothing and leaves linger alone. It prints every
gap a schedule made with the same flags would meet, and whether making it
would ask for consent, and exits 0 either way. A version not installed
yet is no gap (`installed: false`): making the schedule installs it and
checks the inputs it reads then. With `--json` it prints `{ref, version,
digest, modes, ready, gaps, needsConsent, refusals, installed, signedOut,
describe, next}`. Agentry Desktop keeps its Schedule button off
until it says `ready`, then makes the schedule with this binary.

The schedule lives in the runner home under `schedules/<id>/`:
- `schedule.yaml`, the definition;
- `state.json`, what its fires did;
- `fires.ndjson`, one line per occurrence: ran, failed with its exit code,
  or missed;
- `task.txt`, the task, when given with `--task`;
- `keys.cred`, only for a schedule that runs while you are signed out: its
  keys, encrypted (below).

None of these files holds a credential in the clear. Each schedule has one entry in
the system's own scheduler, which runs `agentry schedule fire <id>`. The
entry holds this binary's absolute path and `PATH` and `AGENTRY_DOCKER`
as they were when it was written:
- **Linux:** a systemd user timer and service,
  `~/.config/systemd/user/agentry-schedule-<id>.{timer,service}`.
- **macOS:** a LaunchAgent in your GUI session,
  `~/Library/LaunchAgents/net.secureagentry.schedule.<id>.plist`. Making
  one needs you signed in to the Mac's desktop, not only over SSH.
- **Windows:** a Task Scheduler task, `\Agentry\agentry-schedule-<id>` (in
  the root folder where you may not make `\Agentry\`). It runs as you, only
  while you are logged on, through `conhost --headless`, so no console
  window opens. A task cannot set its program's environment, so the
  pinned paths travel as `--env` arguments.

launchd and Task Scheduler keep only this computer's local time, so their
entries ring on every local time the schedule's own occurrences fall on
(a schedule in another zone, a month's last day, the daylight-saving
changes), and fire runs only what is due.

Fire re-reads the store, so a paused, ended or removed schedule does
nothing. A run that fails is recorded and not retried. Schedules run while
you are signed in, and never wake the computer. An occurrence missed while
the computer was off, asleep or signed out runs once when it is back, if no
later occurrence fell due first; otherwise it is recorded as missed. What
rings for it then is the system's own catch-up: `Persistent=true` on Linux,
the agent's run at sign-in on macOS, a logon trigger and "start when
available" on Windows. `agentry schedule list` and `show` mark a schedule
whose entry is missing, and `agentry schedule sync` rewrites every entry
from the store and removes any entry that has no schedule.

#### Running while signed out (Linux)

On Linux with Docker Engine, `add --signed-out` (or `edit <id>
--signed-out`) also runs a schedule while you are signed out (plan
`quickagent-scheduling.md` §5a, DR-84):

```bash
agentry schedule add acme/digest --daily 06:00 --out ~/reports --signed-out
agentry schedule edit <id> --signed-out=false   # signed in only again
```

- **Linger.** The first such schedule turns on `loginctl enable-linger`
  for you, after you confirm on the terminal (or with `--yes`): your
  schedules then start when the computer starts, even if nobody signs in.
  Your distribution may ask for an administrator's password. agentry
  records that it turned linger on, and turns it off again with the last
  schedule that runs signed out. Linger you turned on yourself is never
  turned off.
- **The keys.** The keychain is locked while you are signed out, so the
  keys a run reads from it are sealed when the schedule is made: the model
  key, your own provider keys that the policy takes from the keychain (AI
  Review, web search, Hybrid Analysis), and the registry token when the
  keychain holds it. They go into an encrypted systemd credential,
  `schedules/<id>/keys.cred`, made by `systemd-creds encrypt --user`, which
  binds it to this computer (its TPM, where there is one) and to you. The
  service unit loads it with `LoadCredentialEncrypted=` and turns the
  keychain off (`AGENTRY_NO_KEYRING`), and the run reads its keys from
  there. Any process running as you can decrypt it, as any process in your
  unlocked session can read your keychain; another user cannot, and a copy
  of the file on another computer does not decrypt.
- **When a key changes.** `agentry setup`, `agentry login` and `agentry
  logout` seal every signed-out schedule's keys again; `agentry schedule
  reseal` does it alone, for a key changed elsewhere (Agentry Desktop's
  Settings run it), and exits 1 if any schedule's keys could not be sealed
  again (`--json` prints `{sealed, failed}`). A schedule whose keys could
  not be sealed again loses the old ones and does not run until `edit <id>
  --signed-out`. The credential is removed when the schedule is removed,
  ends, or stops running signed out.
- **Whether this computer can.** `agentry schedule check-signed-out` says
  whether it can run schedules while you are signed out, and why not
  (`--json`: `{available, reason}`).
- **When it is refused.** On macOS and Windows, and with Docker Desktop for
  Linux (Docker Desktop stops when you sign out); below systemd 256; for a
  QuickAgent whose declared server signs in with an OAuth connection, or a
  kept knowledge pack, whose keys stay in the keychain. There is no
  plaintext fallback.

A signed-out schedule rings on time whether or not anyone is signed in. If
the computer was off, `Persistent=true` rings the missed occurrence once at
boot, and fire's rule above decides whether it still runs.
While linger is on, a schedule made without `--signed-out` still runs only
while you are signed in: when it rings with nobody signed in it takes
nothing, and the next ring while you are signed in deals with the
occurrence by the rule above.

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
enforcement and executes the QuickAgent once, with an output folder for
its reports. The default adapter (`docker-agent`) runs the harness in an
isolated container; `--adapter claude` or `codex` runs your own installed
Claude Code or Codex as a host process behind the same gateway and egress
proxy; `--adapter codex-image` runs OpenAI's Codex in its own signed image
(agentry-dockerimages' Codex image, pinned by digest), the default when your
model is paid by your ChatGPT plan; `--adapter claude-image` runs Anthropic's
Claude Code in an image built on your machine from its npm packages, pinned
by their npm integrity and never published, the default when your model is
paid by your Claude subscription; `--adapter copilot-image` runs GitHub's
Copilot CLI in an image built on your machine from GitHub's release
download, pinned by its published checksum (not signed) and never published,
the default when your model is paid by your Copilot plan (`goose` is
planned, not implemented):

```bash
agentry run demo1/incident-summarizer@^1 --input incident.json --out ./reports
```

One run = verify (digest + DSSE, exactly like install) → compile the
manifest + operator policy into a deterministic run plan → execute:

- **Tools come only from a per-run [Docker MCP Gateway](https://github.com/docker/mcp-gateway)**
  fronting the companion tools server this CLI ships (built locally from an
  embedded context on first use). QuickAgents carry no tool code; an
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
  their capabilities are the gateway's defaults. The tools container's
  `/tmp` is a 256 MiB tmpfs, and so is a release's scratch folder,
  `/scratch` (on Linux, both mounted by the run's docker filter and counted
  inside its memory bound; agentry-cli#599, #729): a write past either
  fails, and nothing reaches the host's disk. A tools server that finds
  either is not a tmpfs refuses to start, and the run stops (exit 13).
  Docker Desktop (macOS, Windows) runs no filter, so there both are the
  container's own layer, unbounded. The receipt names
  what was applied.
- **Under a sandboxed runtime** (`docker.runtime: runsc`, gVisor), the
  containers that hold the QuickAgent's code run under it, and so do the
  tools and MCP server containers the gateway launches (through the run's
  docker filter, on Linux). The llm-proxy, the MCP server install proxy and
  the relays join them when `docker.resolvers` names your host's upstream
  name servers, since a sandbox cannot reach docker's own DNS. The gateway
  joins them when `docker.gatewayRuntime` names a second registration of
  runsc with `--host-uds=open` (agentry-cli#337):
  `docker: {runtime: runsc, gatewayRuntime: runsc-uds, resolvers: [192.168.122.1]}`.
  A runtime the daemon has not registered stops the run (exit 13), and
  `agentry doctor` checks each. The receipt's runtime line names every
  container class and says why any is on the daemon's default.
- **A report can be a PDF.** A release's run (not a built-in's) has
  `export_pdf`, which turns a Markdown report the agent wrote into a PDF
  beside it in `reports/` (agentry-cli#730): when the person asks for one,
  or when the report is for someone else. Typst renders it inside the tools
  container from the runner image's pinned copy (built without a package
  downloader, with the Noto fonts for every script), through the runner's
  `confine` role: an empty environment, a fresh render root it reads from,
  no write of its own where Landlock holds it (the PDF comes back on its
  output), no TCP where Landlock can refuse it, 1 GiB of address space and
  60 seconds; without Landlock (gVisor) it runs as a user that owns
  nothing. Only local PNG and JPEG images are drawn; links stay links
  only for `http`, `https` and `mailto`; Typst code in the Markdown is text.
  The PDF is tagged (PDF/UA-1), its author is the QuickAgent's
  `namespace/name@version`, and its look is the run's report design, never
  the model's choice. `write_report` refuses a PDF, so a PDF in
  `reports/` that the receipt names is one `export_pdf` wrote.
- **The form is the agent's** (agentry-cli#784): `export_pdf` takes
  `form`, one of `report` (the default; the opening heading is drawn as the
  title block, and a titled report of 1,500 words or more gets a cover
  page), `letter` (the opening heading as a letterhead, each short line of
  a paragraph kept, so an address or a signature stays one line to a line,
  and no page numbers on a one-page letter) and `memo` (the opening heading
  as the title, and the To, From, Cc, Date, Subject or Re lines that open
  the body drawn as the memo's header). It takes nothing else: a call that
  names a theme, a colour or a font is refused.
- **The report design is the person's** (agentry-cli#784; the core's
  `pkg/reportdesign`, DEV-230): a theme document, version 2, holding the
  paper (A4 or US Letter), three faces from the fourteen open-licence
  families in the image, one accent colour and its tint, a title block, a
  heading treatment, the tables, the quotes and the footer. Five built-in
  themes are starting points: `plain` (the default, near-monochrome),
  `classic`, `modern`, `editorial` and `technical`. A run takes the first
  that is set: `agentry run --report-design FILE` or `--report-theme NAME`
  for that run alone, then your account's report design when you are
  signed in (chosen on the site or the desktop, read from the registry at
  the start of the run), then `report.design` (a file; a relative path is
  the config directory's) or `report.theme` in
  `~/.config/agentry/config.yaml` (`report: {theme: classic}`), then Plain.
  A name or a file in a flag or the config that is not a design stops the
  run before it starts (exit 2): a theme that is not one of the five, a
  file that breaks one of the design's rules (an accent that cannot carry
  its text among them). An account the CLI cannot reach, or a design it
  does not accept, is a warning and the next choice is used. The receipt's
  `capabilities_enforced["pdf.design"]` names the design, the theme it is
  based on, its digest and what chose it. A thumbnail of each built-in,
  drawn by the same template, is in `pkg/reportthumbs`.
  `agentry report-design` works the account's design from the command line
  (agentry-cli#785): `show` prints the design a run would take now and what
  chose it (`--json` adds the document); `export FILE` writes your
  account's design, or with `--theme NAME` a built-in's, as its canonical
  document (`-` for stdout; an existing file only with `--force`);
  `import FILE` holds a file to every rule of the design, refusing it with
  the field named (exit 2) before anything is sent, and saves it to your
  account; `reset` clears it, back to never chosen. What `export` writes
  and `import` sends is the design's canonical document, so a design
  exported, imported and exported again is the same bytes, and a design
  can be kept in a repository and put back.
- **Everything the run produces** lands in the output folder: the
  QuickAgent's report files, `result.json` (the machine-readable envelope;
  its `pdfs` block names the design and each exported PDF's SHA-256 and
  form),
  and `receipts/` (event stream, gateway log, the exact run plan; for a
  one-shot run, `calls.ndjson`: each runner tool call's outcome, never its
  arguments, and the agent's `end_blocked` sentence, masked like the rest;
  `pdfs.ndjson`, the record of each export).
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
  `run --local` directory is the one exemption: its cached copy is replaced
  from the directory. The record stops an edit by anyone who cannot also
  read or replace the runner home's key; it does not stand against
  someone who can write the runner home itself.
- **`agentry install --local DIR --allow-unsigned`** installs a QuickAgent
  folder that was never published, such as a draft, into the runner home:
  it lists as local and unsigned, `agentry run ns/name` runs it with no
  registry, and every run needs `--allow-unsigned`. The folder is packed as
  `agentry publish` packs it, the contract you confirm is the packed
  manifest's, and that packed copy is installed and sealed like a
  release: unlike `run --local`, a run refuses it once its files change,
  and installing the folder again replaces it (agentry-cli#491).
- **Installed into an existing coding agent instead** (`agentry install
  --target claude-skill|agents-md`), the same contract compiles to the
  host's built-in tool grants — nothing is bundled in either mode.

Exit codes: `0` ok · `1` cli (an interrupted run, an unknown adapter, an
intake error) · `2` usage · `10` verify · `11` policy/secret ·
`12` compile · `13` gateway/docker · `14` agent · `15` timeout ·
`16` budget · `17` output schema · `18` blocked (the agent ended the run
with `end_blocked`, saying why: `reason` `agent-blocked`; or the runner's
tools failed it, every input read or every report failing on the run's own
folders, or the tools unable to write the output folder at all:
`runner-blocked`); `review` adds `20` held for review · `21` rejected ·
`22` refused at Submit · `23` not determined. Task input: `--input FILE`, else piped
stdin; with no task (or an empty one) the agent is sent its own instruction
and runs that. Runs compose: `agentry run a ... | agentry run b ...`.

Around the enforcement core (v2): **`agentry setup`** configures your LLM
provider and stands up the gateway plane before any agent is involved.
**MCP profiles** store a QuickAgent's keys and identities once per exact
release — guided setup on first run, then keep or remove on completion
(`--keep-profile`, `agentry profiles list|remove`). **Connections**: a
remote MCP server declared with `auth: oauth`, or a package server with an
`oauth` block (signed in to at its `issuer`), is signed in to on the
vendor's own consent screen, once per exact release, at the first run that
needs it (on a terminal; the browser opens, or the address is printed and
the address the browser ends on is pasted back), or ahead of it with
`agentry connect ns/name[@version]`. The connection is kept for that
release alone (another QuickAgent or another version asks again), its
refresh token in the OS keychain (a 0600 file under the runner home where
none answers); each run refreshes it under a lock before the sandbox
starts and hands the server an access token alone: a remote server's bridge
in a read-only folder; a package server in the files its `oauth` block
templates, in a folder of its own it may write to (some servers will not
start otherwise), or in an env variable through the gateway's secrets
file. While a run or session lasts, the runner renews a token before it
expires and rewrites the folder (a bridge, and a server that re-reads its
file, pick it up; an env token is fixed for the run). With no
terminal a run that lacks one exits 11 naming the server. `agentry
connections list|remove` manages them (a removal, and an uninstall, revoke
at the vendor where it offers that); `agentry connections client
set|list|remove` keeps your own OAuth client per vendor, which a vendor that
registers no app on the fly needs (Google), with Agentry's steps for making
one shown when it is asked for. A hosted run's host passes
**`--connections-file`** instead: a 0600 JSON file of the access tokens the
deployment refreshed for the run, keyed by server (its address, or a
package server's source id), each `{"accessToken", "expiresAt"}` with an
optional `issuer` and `scopes` for the receipt. Nothing is consented to,
refreshed or renewed and no connection is kept; a token that expires during
the run ends that server's calls with a sentence naming the connection
(agentry-cli#722). **Model sign-ins** (agentry-cli#738, the engine of
the model sign-ins plan) let a run spend a plan you hold instead of an API
key: the policy's `apiKeyKind` names `chatgpt-plan`, `claude-signin` or
`copilot-signin`, and the grant is kept per person under the runner home
(`model-signins/`, its refresh token in the OS keychain, or a 0600 file
where none answers), listed and
removed by `agentry model-signins list|remove <vendor>`. **Sign in with
ChatGPT** (agentry-cli#739): `agentry model-signins add chatgpt` (or `agentry
setup`, provider `openai`, paying with `chatgpt`) signs in in your browser
with OpenAI's plan usage, checks that the grant allows it
(`chatgpt.tokens.use.direct`) and that your plan does (an ineligible account
is told to use an API key and nothing is kept), and lists your plan's models;
`remove chatgpt` revokes the grant at OpenAI. Such a run goes to
`api.openai.com:443` alone and runs on the Codex image by default, or on your
installed Codex with `--adapter codex` (not on Windows yet); both run Codex's
app-server, an autonomous run as its one turn. Codex reads the token file
whenever it needs a token (`harness.<adapter>.signInTokenRoute: auth-command`,
the default), or, with `restart`, reads it once at start and, in a session,
is started again on a renewed token and resumes the conversation (OpenAI's
documented route). A run is handed an
access token alone, in one file of its own mounted read-only into the
agent container and rewritten before it expires, or the harness's own
config directory; it reports tokens, never dollars, refuses `--max-cost`,
and ends with the plan-limit outcome (exit 16, reason `plan-limit`) when
the plan's limit is reached (a 429 on a ChatGPT plan, never retried). A
hosted run's host (a run grant, `--connections-file` or `--session-protocol`)
is refused one. **Use your Claude subscription (runs Claude Code)**
(agentry-cli#740): Anthropic does not let another program run or keep a
Claude sign-in, so `agentry model-signins add claude` (or `agentry setup`,
provider `anthropic`, paying with `claude`) starts the unmodified Claude
Code's own `claude auth login` in a config directory of Agentry's own, never
your `~/.claude`, opens the address Claude Code prints, and keeps only where
that directory is and the account Claude Code reports; Claude Code keeps and
refreshes its sign-in there and Agentry never reads it. It signs in the
Claude Code image (a docker volume, `agentry-claude-signin`; the browser
cannot reach Claude Code's callback inside the container, so you paste the
code Anthropic's page shows at Claude Code's prompt), the default for such a
run; `--installed` signs in your installed Claude Code instead (a directory
under `model-signins/`, with Claude Code's own browser and callback), for
`--adapter claude`. The image is built at first use from
`@anthropic-ai/claude-code` and its native package for your platform, each
checked against the npm integrity this build carries (a mismatch builds
nothing). A Claude run gets that directory read-write and nothing else of
the sign-in, no `--bare`, no API-key variable, no settings, the gateway as
its only tool server, and its egress proxy allows `api.anthropic.com:443` and
`platform.claude.com:443` alone (the image's only way out; the installed
Claude Code, a process on your computer, is asked to use it, as any host
adapter is); a `rate_limit_event` Claude Code reports as
rejected ends it with the plan-limit outcome and Claude Code's reset time.
`remove claude` runs Claude Code's own logout there and deletes the
directory. The API-key choice stays: the Max and Team plans include monthly
API credits, spent through a key from the Console organization linked to the
plan. **Sign in with GitHub** (agentry-cli#741): `agentry model-signins add
copilot` (or `agentry setup`, provider `copilot`) shows a code to enter on
GitHub's own device page and signs in through Agentry's GitHub App
(`--oauth-app`: its OAuth App instead), with no client secret anywhere; the
refresh token is kept, and `remove copilot` submits the grant's tokens to
GitHub's credential revocation, which accepts them and acts after it answers
(GitHub tells the account's owner; Agentry's app stays in your authorized apps
until you remove it there). Until this
build carries Agentry's public client ids (agentry-cli#743),
`AGENTRY_GITHUB_APP_CLIENT_ID` or `AGENTRY_GITHUB_OAUTH_APP_CLIENT_ID` names
one. Such a run uses the Copilot CLI image, built at first use from
`copilot-linuxmusl-<arch>.tar.gz` of a release this build pins: its
`SHA256SUMS.txt` is held to the pin and the download to the checksum it names
(a mismatch builds nothing), the package unpacked into the image at build.
Copilot CLI runs in its JSON-RPC server mode, driven by the runner: the
access token goes to it through its token callback over its own stdio
(never its environment, a file or the plan), renewed when it asks; the run's
instruction replaces GitHub's system prompt; the gateway is its only MCP
server and `--available-tools mcp:*` its only tools; telemetry, custom
instructions, skills, memory and auto-update are off; the runner approves
each tool call of the run's own. Its egress proxy allows `api.github.com:443`
and the Copilot API host of your plan alone (`setup --copilot-plan
individual|business|enterprise`; GitHub's telemetry and experiment hosts are
denied). A quota or rate-limit error ends it with the plan-limit outcome; an
organization's policy that turns Copilot CLI off for your account, or
requires a person to approve each tool call, ends it refused by that policy
(exit 11, reason `org-policy`, the policy named).
**Connect your account** (agentry-cli#742): for OpenRouter, Poe and Hugging
Face, `agentry model-signins add openrouter|poe|huggingface` (or `agentry
setup`, the provider, paying with `connect`; `--connect` without the
question) has the vendor make a key for Agentry in your browser, billed to
your account there, and keeps it in the OS keychain where a pasted key is
(`provider:<provider>`); it is not a model sign-in, and a run spends it as it
spends any key (its variable still wins when it is set). OpenRouter's PKCE
flow mints an `sk-or-` key; Poe's Sign in with Poe (scope `apikey:create`,
through Agentry's Poe client, which until agentry-cli#743 `AGENTRY_POE_CLIENT_ID`
names) mints a key with the expiry you choose on Poe's page, after which a
run is refused with the fix; Hugging Face grants an `inference-api` token to
Agentry's client metadata document (agentry-web#1331; `AGENTRY_HF_CLIENT_ID`
names a public app instead), renewed at each run's start from the refresh
token kept beside the other sign-ins. You type the model id (no list is
offered): an OpenRouter id such as `anthropic/claude-sonnet-4.5`, a Poe bot
such as `Claude-Sonnet-4.6`, a Hugging Face id that may name its provider,
such as `openai/gpt-oss-120b:groq`; a model without tool calling fails. Runs
use docker-agent: OpenRouter's and Hugging Face's own providers, Poe as a
named OpenAI-compatible provider pinned to Chat Completions; each reaches its
vendor's API host alone (`openrouter.ai:443`, `api.poe.com:443`,
`router.huggingface.co:443`). `remove` forgets the key and says where to
delete it at the vendor (OpenRouter and Poe give apps no way to revoke one;
Hugging Face's is revoked where its discovery names a revocation endpoint,
none as of 2026-10-09). A provider that answers 402, out of credits, ends any
run with that outcome (exit 16, reason `out-of-credits`) and the provider's
words. A connect needs the OS keychain (not under `AGENTRY_NO_KEYRING`), and a
signed-out schedule spends the copy of the key sealed for it, which is not
renewed.
Harness images are pinned per adapter: docker-agent's by digest
(`images.harnessDockerAgent`), any other's in `images.harnessImages` by
digest or by the vendor artifact it is built from, and the receipt names
the pin and the credential kind. **`--file`** hands a
run external files read-only under `/inputs`. Before the agent starts the
runner decodes them (a document, PDF or image into its text, and an
archive into its files, each decoded in turn, in the files image, with no
network; `agentry files ensure` pulls it ahead of time), and
the run reads them through `list_inputs` (each file's type, size, whether
it could be read and its views), `read_input` and `search_inputs`. Each
reads a file in one of two views: `text`, a document's decoded text and the
default for documents, or `original`, the file as uploaded and the default
for everything else (code, markup and data pass through unconverted); a
binary original comes as pages of base64 with its size and type, and HTML's
`text` view is pandoc's Markdown. A release whose manifest declares
`inputs.view: text` or `original` (core DEV-218) has every input that
has that view read in it by default instead, and the System Prompt says so;
`both`, or no declaration, keeps each file's own default (agentry-cli#693).
No QuickAgent needs `exec` to read its inputs (agentry-cli#631). The receipt's `decode` line says what was read. **Every file, and every file in
an archive, is checked by ClamAV before its text is extracted**, in the same
container, against a signature bundle the CLI refreshes once a day from
`ghcr.io/blaktron/agentry-clamav-db` (about 110 MB the first time, the day's
changes after) and verifies against the signature of the workflow that
publishes it, before using it. A file ClamAV finds refuses the run before
the agent starts, with exit 11, naming the file and what was found; in a
session, a file you add that ClamAV finds is refused alone and the session
goes on. The `decode` line names the signatures' date and what was clean,
found or not checked (a file past 256 MB, or what an archive holds past
that, is not checked, and says so). With no signatures yet (a first run
with no network) the run goes on and the receipt says nothing was checked;
signatures more than 7 days old are named stale. **The risky files among
them are also looked up on Hybrid Analysis (CrowdStrike) by their SHA-256
alone, never uploaded**: executables and libraries, installers, `.jar` and
`.apk`, scripts (`.js`, `.vbs`, `.ps1`, `.bat`, `.cmd`, `.hta`, `.wsf`),
shortcuts and disk images, legacy and macro Office, RTF, PDF and CHM, by
the type detected in the files image and by name, archive members
included. The CLI process asks, never a container. A malicious verdict
refuses the run before the agent starts, with exit 11, naming the file and
its threat score (a session's added file alone); a suspicious one is noted
in the `decode` line, which names each outcome: clean, noted, refused, or
not checked with the reason (`no key`, `quota` after a minute of rate
limits, `unavailable`, `the key was refused`), and the run goes on. The key
is the runner policy's `hybridAnalysis` (the variable `$HYBRID_API_KEY` by
default, or `hybridAnalysis.apiKeyEnv`; with `apiKeySource: keyring`, the
keychain entry `provider:hybrid-analysis`, which the desktop's Providers
settings are to write, agentry-desktop#530); with none, nothing is asked
and the line says `not checked: no key`. A sandbox host hands its CLI
Agentry's key in `$HYBRID_API_KEY` (agentry-enterprise#332), so a hosted
run looks up once its host carries a CLI with this check. **A hosted run's
inputs are also read by AI Review** (agentry-cli#321): with
**`--review-inputs REGISTRY`** and the job's credential in
`$AGENTRY_JOB_TOKEN`, which a sandbox host passes, every text the agent
could read of them (a document's decoded text, and its original when that
is text too, HTML as uploaded and as Markdown, each file in an archive) is
sent to that registry's model-input scan as `input_file` before the agent
starts, several small texts to a call. An input AI Review flags, or one it
could not read, refuses the run with exit 11, naming the file (the
reason's detail `ai-review` or `not-reviewed`); in a session, a file you
add is refused alone. AI Review reads at most 16 MiB of text a run: a file
past that is not refused but withheld, listed by `list_inputs` as not read
with no view, and the run goes on with the rest. A binary original, which
`read_input` serves as base64, is not sent. The receipt's `inputReview`
line says how much was sent, in how many calls, with which question set,
and names any file withheld. A run on your own computer is not reviewed. A zip, tar, 7z or rar (or a
gzip, bzip2, xz or zstd stream) is unpacked and each file in it is listed
and read by its path inside it, `archive.zip/folder/file.docx`: at most
three archives deep, 2,000 files and 512 MiB unpacked in all. Links,
devices and encrypted members are listed as unread, and an archive with a
member whose path is absolute, climbs out with `..` or is not a plain name
is not unpacked at all. A run with no files pulls
nothing. **`--knowledge NAME`** (a
pack kept on this computer) or **`--knowledge DIR`** attaches a knowledge
pack — an LLM wiki of at most 10 MB with the documents it was compiled
from — read-only at `/knowledge/<name>`, which the run searches with
`knowledge_list`, `knowledge_search` and `knowledge_read`; the receipt
names each pack by digest and says whether it was scanned. Attaching a pack changes neither the plan nor its hash.
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

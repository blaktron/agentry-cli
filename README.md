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

`init` · `validate` · `login` · `whoami` · `publish` · `install` · `list` ·
`setup` · `doctor` · `files` · `run` · `knowledge` · `voice` · `draft` · `review` · `builtins` · `uninstall` · `profiles` · `verify` ·
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
   calls malicious (looked up by its SHA-256 alone, as for a run). PDF uses `pdftotext`; DOCX, ODT, EPUB, RTF and HTML
   use `pandoc` to GitHub Markdown; images use `tesseract`'s English OCR.
   XLSX, ODS and CSV become capped Markdown tables; PPTX and ODP become a
   section per slide. Legacy Office, Outlook mail and other formats use
   Apache Tika. UTF-8 text and JSON pass as they are; a file no reader can
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
   stripping what the task needs. A last message that was not a draft at all (not one JSON object,
   exit 17) gets the same turn, given the error and that message back. It returns the manifest again, and the instruction file too if
   the report names it. The second answer is corrected and checked like the
   first, and kept whether or not it passes; its notes say "Repaired once"
   and what the rules said of the first. A repair turn that returns no draft
   leaves the first, with its problems and a note saying why, or, when the
   first was not a draft either, ends with exit 17 as before. The turn's
   budget is the draft's, capped at 300 seconds and 8 tool calls, and what
   is left of `--max-cost`. Its receipts are in `DIR/repair/`.

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
corrected (`corrections`), whether it was repaired (`repaired`), and AI
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
  sentence before the object, say), the object fenced inside it is taken if
  the schema passes it, and the reading is marked `recovered`.
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
proxy (`goose` is planned, not implemented):

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
  their capabilities are the gateway's defaults. The receipt names what
  was applied.
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
- **Everything the run produces** lands in the output folder: the
  QuickAgent's report files, `result.json` (the machine-readable envelope),
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
with `end_blocked`, saying why, or every one of its input reads or reports
failed, or the runner's tools could not write the output folder: the
envelope's `reason` is `agent-blocked` or `runner-blocked`); `review` adds `20` held for review ·
`21` rejected · `22` refused at Submit · `23` not determined. Task input: `--input FILE`, else piped
stdin; with no task (or an empty one) the agent is sent its own instruction
and runs that. Runs compose: `agentry run a ... | agentry run b ...`.

Around the enforcement core (v2): **`agentry setup`** configures your LLM
provider and stands up the gateway plane before any agent is involved.
**MCP profiles** store a QuickAgent's keys and identities once per exact
release — guided setup on first run, then keep or remove on completion
(`--keep-profile`, `agentry profiles list|remove`). **`--file`** hands a
run external files read-only under `/inputs`. Before the agent starts the
runner decodes them (a document, PDF or image into its text, and an
archive into its files, each decoded in turn, in the files image, with no
network; `agentry files ensure` pulls it ahead of time), and
the run reads them through `list_inputs` (each file's type, size and
whether it could be read), `read_input` (the text) and `search_inputs`; the
receipt's `decode` line says what was read. **Every file, and every file in
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
run looks up once its host carries a CLI with this check. A zip, tar, 7z or rar (or a
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

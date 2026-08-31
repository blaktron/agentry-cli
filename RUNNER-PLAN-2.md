# RUNNER-PLAN-2 — BriefAgent runner v2: setup, MCP profiles, inputs, providers

**Status:** IMMUTABLE once shipped; corrections go to `RUNNER-DEVIATIONS.md`.
**Parent:** RUNNER-PLAN.md (v1, unchanged and still binding). This plan adds the
operator-experience layer around the v1 enforcement core: first-class setup, persistent
per-BriefAgent MCP profiles with guided credential configuration, a read-only run-inputs
channel, per-provider install paths, and the desktop run surface. Seven operator
requirements (2026-08-31) drive it; each section names the requirement it satisfies.

## P1. `agentry setup` — the gateway from the very start (req 1, req 5)

One idempotent command prepares a machine for BriefAgent runs *before* any agent is
involved:

```
agentry setup [--provider anthropic|openai|google|custom] [--model NAME]
              [--api-key-env VAR] [--base-url URL] [--yes]
```

1. Verifies the docker engine answers.
2. Writes (or updates) the operator policy, with the **LLM provider configured up front**:
   interactive on a TTY (provider → default model → key env var → proxy allowlist derived
   per provider), flags/defaults under `--yes`. Editing an existing policy preserves
   everything outside the `model:` block. Provider defaults:
   | provider | model default | apiKeyEnv | proxyAllow |
   |---|---|---|---|
   | anthropic | claude-sonnet-4-5 | ANTHROPIC_API_KEY | api.anthropic.com:443 |
   | openai | gpt-5.2 | OPENAI_API_KEY | api.openai.com:443 |
   | google | gemini-3-pro | GEMINI_API_KEY | generativelanguage.googleapis.com:443 |
   | custom | asked | asked | derived from --base-url (host:port) |
3. Ensures the docker-mcp-gateway plane immediately (pull pinned gateway + harness
   images, build the runner image from the embedded context) — the same
   `gateway.EnsureInstalled` the first run would lazily perform, now front-loaded.

`agentry run` continues to self-heal (lazy ensure) so setup is a convenience, not a gate.
The logic lives in `pkg/runner/setup` behind a small Prompter interface so the desktop
app drives the same code (§P6).

## P2. MCP profiles — one per BriefAgent signature (req 1)

A **profile** is the persistent, credentialed half of a BriefAgent's gateway
configuration: the application keys and identities its declared contract needs, stored
once per **signature** (ns/name@version + archive digest — the same identity the install
cache uses) and reused by every subsequent run of that exact release.

```
~/.agentry/runner/profiles/<ns>/<name>/<version>-<digest12>/profile.json   (dir 0700, file 0600)
```

`profile.json` (schema `profileVersion: "1"`): ref, digest, `keep` (bool), createdAt,
lastUsedAt, `secrets: {ENV_NAME: value}` — exactly the manifest-declared names. Values
live only here and in the per-run gateway secrets file (0600, deleted at teardown); they
never enter plans, receipts, or process listings (v1 invariants hold).

Lifecycle:
- **Created** by guided setup (§P3) on the first run/prepare of a signature.
- **Consumed** by every run: secret resolution order becomes **profile → host env /
  envFile** (policy source stays the fallback so CI without profiles works as in v1).
- **Completion**: `keep: true` → retained (lastUsedAt stamped) so the next run of the
  same version "just uses the authenticated resources"; `keep: false` → the profile
  directory is deleted when the run completes. The choice is configurable at install
  time: `--keep-profile=<bool>` on `agentry run` (covers `--prepare`, the install step),
  the guided setup's final question, and the policy default `profiles.keepByDefault`
  (shipped default **true**).
- **Managed**: `agentry profiles list` and `agentry profiles remove <ref|--all>`.

Honest naming note: docker's native `docker mcp profile` store (feature-flagged,
per-user SQLite) is deliberately *not* used — annex §4.1 records why hermetic per-run
file generation won. An agentry MCP profile is agentry-level state that feeds the same
generated gateway file set v1 defined; the receipt's enforcement text is unchanged.

## P3. Guided profile setup (req 2)

When a run (or `--prepare`) finds **no profile** for the signature and the manifest
declares secrets, an interactive guided setup runs on a TTY:

1. Shows ref, digest, and the capability contract (what the keys are FOR).
2. Prompts for each declared secret by name, input hidden (`/dev/tty`, echo off;
   plain-with-warning fallback when raw mode is unavailable). Empty input = "resolve
   from the environment at run time" (recorded as absent, not empty).
3. Asks "Keep this profile for future runs of this version? [Y/n]" → `keep`.
4. Writes the profile and proceeds.

Non-interactive behavior is v1's: resolve from the policy secret source; missing ⇒ exit
11 naming the variable. `--reconfigure-profile` forces the guided flow over an existing
profile; `--keep-profile` set on the command line skips question 3. A signature with no
declared secrets creates no profile (nothing to keep) unless `--keep-profile=true` is
passed explicitly, which records the keep choice for future schema growth.

## P4. The run-inputs channel (req 7)

Files and artifacts from outside the container reach a run through a dedicated
read-only channel, separate from the workspace and from the output volume:

- `agentry run --file PATH` (repeatable; files or directories) copies each named item
  into the per-run `runs/<id>/inputs/` directory (basename-rooted; directories
  recursively).
- The compiled catalog always mounts `@@RUN_INPUTS@@:/inputs:ro` into the **tools**
  container; the engine substitutes the placeholder and creates the directory (empty
  when no `--file` was given).
- The tools server always grants two additional tools, mirroring `write_report` as the
  operator-owned I/O channels: **`list_inputs`** and **`read_input`** (rooted at
  `/inputs`, path-safe, read-only by mount and by code). They are task-input plumbing —
  operator-provided material, like stdin — not part of the manifest capability
  vocabulary, so the capability contract is untouched.
- The runner-contract postscript tells the agent: input files, when provided, are under
  `/inputs` — list with `list_inputs`, read with `read_input`.
- Job output and receipts stay on the separate `/out` volume exactly as v1 defined.

The agent container still mounts only `/plan` (v1 §R11 invariant 2): "mounted into the
BriefAgent's container" is realized at the tool plane, where every file the model can
touch already lives; the enforcement story is unchanged and the receipts still name it.

## P5. Per-provider installs (req 6)

Embedded mode grows provider-named entry points so a Claude Code / Codex / OpenClaw
user installs BriefAgents that run on **that application's** MCP servers and built-in
tools — never through the docker gateway path:

```
agentry install <ref> --provider claude    # = --target claude-skill
agentry install <ref> --provider codex     # = --target agents-md
agentry install <ref> --provider openclaw  # = --target claude-skill (OpenClaw consumes
                                           #   Claude-format skills today)
```

`--provider` and `--target` are mutually exclusive; the capability→host-tool mapping
(v1 §R5) rides along unchanged. The web **Downloads page** lists these flows per
provider next to the CLI/desktop downloads; docs carry the same table. Distribution
through each provider's plugin system is recorded as backlog, not v2 scope.

## P6. Site + desktop surfaces (req 3, req 4)

- **Site (agentry repo, web):** every BriefAgent page gains an **"MCP servers & tools"**
  section derived from the manifest: the tools the runner grants (v1 tool table + the
  always-on input/report channels), the MCP servers involved (the BriefAgent Tools
  companion server; operator-cataloged servers per declared dataAccess category), the
  declared secret names (with a note that guided setup collects them into the
  BriefAgent's MCP profile), and the embedded-mode equivalent (host built-ins).
- **Desktop (agentry-desktop):** the registry mirror (Browse) reaches parity with the
  site — including the MCP servers & tools section — and adds what the site cannot:
  **install and run from the app**. A `RunAPI` (Wails-bound Go service over
  `agentry-cli/pkg/runner`) exposes: setup status + one-click setup (§P1), profile
  status + guided configuration as a form (§P3, with the keep choice), and
  run-with-task (task text, optional input files → §P4, output folder picker),
  streaming progress events and finishing with the result envelope + a link to the
  output folder. LLM provider preference is a Settings surface writing the same policy
  (req 5). Requires bumping the desktop's agentry-cli pin to the v2 commit.

## P7. Acceptance (extends v1 scripts)

- r0 additions: `--file` inputs appear in the plan (inputs volume + tools granted);
  plan bytes stay deterministic (inputs are run-scoped placeholders, not plan content).
- r1 additions: `list_inputs`/`read_input` over MCP stdio — listing, reading, traversal
  refusal, and empty-inputs behavior.
- New r-profile script (no docker): profile create → keep semantics → resolution
  precedence (profile beats env) → remove-on-completion — driven through `--dry-run` +
  a fake run where possible, unit tests otherwise.
- Setup: `agentry setup --yes` on a docker host = pull/build + policy written (r2
  extension, docker-gated).

## P8. Decisions

Profile identity = ref+digest (a re-published different digest is a different trust
decision → fresh guided setup). Default keep = true (authenticated resources are the
point; discard is the explicit choice). Input files always readable when provided
(operator channel, like stdin — not a manifest capability). OpenClaw maps to the
Claude-skill format until its plugin system lands. Desktop drives the same
`pkg/runner`/`pkg/runner/setup` code as the CLI — no parallel implementations.

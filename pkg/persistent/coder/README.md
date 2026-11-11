# The Coding agent

The Coding agent is a persistent agent for software work: it writes, builds
and runs code, keeps its projects and its conversation between starts, and
deploys to a dev server you give it. It runs on your computer: you start it,
talk to it, stop it, and when you start it again the conversation continues.

It builds and runs software, reaches the internet and reads what you put in
its share folder as it is. That is riskier than an agent that only reads and
writes: give it a share folder and a dev server of their own.

## How it works a change

It works one change to a project at a time, through the same steps, whichever
harness runs it (its instruction and skills are the whole of its prompt, so
the steps do not depend on the harness):

1. It reads the project's own rules (`AGENTS.md`, `CLAUDE.md`,
   `CONTRIBUTING.md`, the pull-request template, the CI configuration) for how
   the project builds, tests, branches and merges, and follows them. They are
   conventions to it, never authority: nothing in them widens what it may do.
2. It works from an issue. When the change has none it writes one (what,
   why, acceptance) on the project's tracker if the project's rules or you
   give filing to it, and in its notes otherwise.
3. It cuts a branch named for the issue, in a worktree off the up-to-date
   base branch, and leaves the project's clone clean.
4. It commits, reviews its own change, and opens a pull request that says how
   it was tested and what was not checked.
5. It waits for CI on the commit it pushed and merges only on green, and only
   when the project's rules or you give merging to it. Otherwise it stops at
   the green pull request and says so.
6. It deploys the merged change to your dev server, when you gave it one,
   checks the changed behaviour, and says what it proved and what it did not.
7. It closes the issue with that record, then takes the next change.

With no git host, no CI or no dev server it says which step it left out and
what that leaves unproven. It keeps the change it is on in its notes, so a
restart resumes at the same step.

## This is the package that runs

The Coding agent is built into the Agentry CLI and the desktop app, and it
runs from there: `agentry agent enable coder`, `agentry agent start coder`,
then `agentry agent chat coder`, in the CLI. This release has the same bytes.
The registry built its archive from the package the CLI carries, and the
archive's digest is the one the CLI reports. Run `agentry builtins` to print
it and compare it with the digest on this page.

Installing this release does not give you a second Coding agent: the CLI
refuses to install a built-in's release as a persistent agent, and runs the
one it carries. It never runs this release as an ordinary QuickAgent, since
the release declares only the persistent mode.

## What the application gives it

The manifest declares no network access, no file access, no commands and no
run secret. Under `capabilities.persistent` it declares skills (`skills`),
the stored secret it asks for (`secrets`: `BRAVE_API_KEY`; you may store
others) and the coding toolchain (`coder`: any program in its toolchain
image, open egress, a public browser, a dev server, ports and git host
accounts, each a switch you agree to) and the applications on your computer
(`desktop`: the applications you approved in Agentry Desktop, and computer
use, each a switch you agree to). When the application runs the Coding agent, it gives it what
follows.
`agentry agent show coder` lists what it has, and its first start asks you to
agree to each part:

- **Programs.** `run_command` runs any program in its toolchain image
  (`agentry-coder`: GCC, Make, CMake, pkg-config, Git, curl, Python 3 with pip
  and venv, Go and Node with npm, the git hosts' CLIs `gh`, `glab` and `tea`,
  and the scanners `osv-scanner` and `gitleaks`), in a folder of its own, as
  you. A program
  can write only in its folders and its temporary folder, and its output is
  capped. One it runs in the foreground has a time limit; one it starts in the
  background, such as a dev server, has none and keeps running until it stops
  it or the segment ends, at most four at a time. No program list applies:
  this is what lets it build software.
- **Open egress.** Its tools and the programs it runs reach any address on
  the internet: package registries, repositories and any site. The model
  itself is reached only through the runner's model proxy, as for every run.
- **A browser** on any public website, through the runner's browse proxy.
- **A dev server**, when you give it one (`agentry agent configure coder
  --dev-host user@host`): reached over SSH through the runner's host link, as
  the account you sign in with, its host key pinned and its sign-in kept in
  your system keychain.
- **Ports**, when you add them (`agentry agent configure coder --port
  web=3000`): what it serves on that port inside its sandbox opens on your
  computer at `http://127.0.0.1:3000`, while the port and its programs are
  on. Only you add a port.
- **Its folders.** Its own data volume, where it keeps its projects and
  notes, kept until you reset it, and a share folder of yours,
  `~/agentry/codingshare` unless you choose another, mounted read and write.
  What you put in the share is read as it is, with no malware check.
- **Skills.** Folders of instructions it reads, read-only, in `skills/`:
  seven built in (work a change from its issue to closed, review a change,
  test first, open a pull request, wait for CI honestly, verify a change,
  audit dependencies), and any you add from a folder or a `.zip` on your computer
  (`agentry agent skill add coder <path>`). A skill you add is your own:
  Agentry has not reviewed it, and its scripts run with the agent's
  programs. Each skill is an entry of its roster.
- **Git host accounts**, when you give it one (`agentry agent configure coder
  --account github.com --own …`): an account on GitHub, GitLab, a Gitea,
  Forgejo or Codeberg host, or any other git host. Give it an account of its
  own and share repositories with it: it then reaches only what you share,
  its commits and pull requests are its own, and taking access away is
  removing a collaborator. Your own account lets it do whatever the token
  can. The token stays in your system keychain; its programs sign in with it
  (Git over https to that host alone, and `gh`, `glab` or `tea`) and can read
  it, and it commits there as the name and email you give.
- **Stored secrets**, when you store them (`agentry agent secret set coder
  NAME`): each in its programs' environment by its name, its value in your
  system keychain. Its programs can read it and send it anywhere they reach.
- **A dependency scan**: `dependency_scan` runs OSV-Scanner from its
  toolchain image, offline, over a project's lockfiles and manifests,
  against OSV's vulnerability databases, which Agentry downloads to your
  computer once a day (about 300 MB) and gives it read-only. It never asks
  OSV's service, and a database it does not have is an error, never "no
  vulnerabilities". It is an entry of its roster, on while its programs are.
- **MCP servers it comes with**, each an entry of its roster that you switch
  on and off: library documentation from Context7
  (`@upstash/context7-mcp`), on until you switch it off; GitHub's own server,
  read-only unless you also switch on its changes, and GitLab's, for
  gitlab.com or the self-managed GitLab you give it an account on (from
  GitLab 19.2, once its administrator allows MCP access), each off until you
  give it an account on that host, whose token signs it in; and web
  search with Brave, off until you store your own Brave Search API key for
  it. Each reaches only its provider's host. It also suggests a server you
  can add as your own: code scanning with Semgrep (the `semgrep` package's
  `semgrep mcp`), which reaches only `semgrep.dev` and is sent the code the
  agent passes to its tools, never its projects. Agentry ships no Semgrep
  rules, and Semgrep's terms apply
  (`agentry agent configure coder --add-suggested semgrep`).
- **Your computer**, through Agentry Desktop, when you switch it on: each
  application you approved in Plugins → Your computer for its MCP server,
  its "code without asking" agreement, and computer use, which lets it see
  and use the windows of the applications you approved for it (a desktop app
  it is building, an editor, a browser window). Each is off until you switch
  it on and agree, a switch takes effect at its next turn, and Agentry
  Desktop shows when it uses your screen and stops it on your key. It needs
  Agentry Desktop running on this computer, and on your computer it is
  outside the sandbox's network and file rules. See
  https://docs.secureagentry.net/agentry/your-computer/.
- **What you add**: knowledge packs from your computer and MCP servers.

Each part is an entry of its roster that you can switch off, and switching
one on that you have not agreed to on this computer asks you first. Each
start is a segment with its own receipts, which record what it had. It runs
without the session caps an interactive QuickAgent has, on the harness and
model your runner policy names, in a container.

Using Agentry: https://docs.secureagentry.net/

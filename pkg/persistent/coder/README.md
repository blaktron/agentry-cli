# The Coding agent

The Coding agent is a persistent agent for software work: it writes, builds
and runs code, keeps its projects and its conversation between starts, and
deploys to a dev server you give it. It runs on your computer: you start it,
talk to it, stop it, and when you start it again the conversation continues.

It builds and runs software, reaches the internet and reads what you put in
its share folder as it is. That is riskier than an agent that only reads and
writes: give it a share folder and a dev server of their own.

## This is the package that runs

The Coding agent is built into the Agentry CLI and the desktop app, and it
runs from there: `agentry agent enable coder`, `agentry agent start coder`,
then `agentry agent chat coder`, in the CLI. This release has the same bytes.
The registry built its archive from the package the CLI carries, and the
archive's digest is the one the CLI reports. Run `agentry builtins` to print
it and compare it with the digest on this page.

Installing this release does not give you the Coding agent. A persistent
release runs only as a built-in, so the application runs it, with what is
listed below; the runner refuses to run this release as an ordinary
QuickAgent.

## What the application gives it

The manifest declares no network access, no file access, no commands and no
secret. When the application runs the Coding agent, it gives it what follows.
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
  four built in (review a change, test first, audit dependencies, open a
  pull request), and any you add from a folder or a `.zip` on your computer
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
- **What you add**: knowledge packs from your computer and MCP servers.

Each part is an entry of its roster that you can switch off, and switching
one on that you have not agreed to on this computer asks you first. Each
start is a segment with its own receipts, which record what it had. It runs
without the session caps an interactive QuickAgent has, on the harness and
model your runner policy names, in a container.

Using Agentry: https://docs.secureagentry.net/

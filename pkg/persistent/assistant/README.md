# The Assistant

The Assistant is a personal assistant that keeps its conversation with you
across sessions. It runs on your computer as a persistent agent: you start
it, talk to it, stop it, and when you start it again the conversation
continues.

## This is the package that runs

The Assistant is built into the Agentry CLI and the desktop app, and it runs
from there: `agentry agent start assistant`, then `agentry agent chat
assistant`, in the CLI. This release has the same bytes. The registry built
its archive from the package the CLI carries, and the archive's digest is the
one the CLI reports. Run `agentry builtins` to print it and compare it with
the digest on this page.

Installing this release does not give you the Assistant. A persistent release
runs only as a built-in, so the application runs it, with what is listed
below; the runner refuses to run this release as an ordinary QuickAgent.

## What the application gives it

The manifest declares no network access, no file access, no commands and no
secret. When the application runs the Assistant, it gives it what follows,
and `agentry agent show assistant` lists what it has:

- **Its conversation is kept** on a volume of the agent's own, which the
  runner keeps between starts and removes only when you reset the agent
  (`agentry agent reset assistant`).
- **Its own files** are on a second volume, its data folder, kept the same
  way.
- **Its memory** is on a third volume, kept the same way: a wiki it writes in
  `memory/`, with pages on what you tell it and what you share with it, each
  fact with its source (a file in your share and the date it read it, or what
  you said and when), an index and a log. It is told never to change your
  share's files to keep it. When the memory is made, the notes in its data
  folder's `notes/` are copied into it, and stay where they are. `agentry
  agent memory export assistant <folder>` copies the memory into a new
  folder, readable by you alone; a reset removes it.
- **Eleven skills** of its own, on unless you switch them off: `memory-ingest`,
  `memory-query` and `memory-lint`, which say how it adds to its memory,
  answers from it and checks it; `google-workspace-setup`, which walks you
  through connecting your Google account; `google-workspace`,
  `google-inbox-triage` and `google-meeting-prep`, adapted from gog's own
  skills (MIT, their licence beside them), which say how it uses your Google
  account safely; `microsoft-365-setup`, which walks you through connecting
  your Microsoft account with an Entra app of your own; and `microsoft-365`,
  `microsoft-inbox-triage` and `microsoft-meeting-prep`, which say how it
  uses your Microsoft account safely.
- **A share folder** of yours, `~/agentry/assistantshare` unless you choose
  another, is mounted read and write while it runs: you see what it writes
  there as it writes it. A file you add is checked for malware with ClamAV
  before it reads it, and one ClamAV finds is not read; a file the check
  cannot read (no signatures on your computer, or past its size limits) is
  read unchecked, and the segment's receipts say so. You can switch the share
  off.
- **MCP servers it comes with**, each an entry of its roster that you switch
  on and off: Wikipedia (`@cyanheads/wikipedia-mcp-server`, English
  Wikipedia alone), on until you switch it off, and web search with Brave
  (`@brave/brave-search-mcp-server`), off until you store your own Brave
  Search API key for it (`agentry agent secret set assistant BRAVE_API_KEY`).
  Each reaches only its provider's host. It also suggests two servers you can
  add as your own, for your own use under their providers' terms: weather
  from Open-Meteo, whose free service is for non-commercial use, and maps
  from OpenStreetMap, whose public servers ask for light use
  (`agentry agent configure assistant --add-suggested weather` or `maps`).
- **Google Workspace** through gog's MCP server, run from the image Agentry
  pins, off until you switch it on and sign it in with your own Google Cloud
  OAuth client (`agentry agent configure assistant --google-client <file>`,
  then `--google-sign-in <email>`): it reads Gmail, Drive, Docs, Sheets and
  your Calendar's events, read only unless you also switch on its changes,
  and sends mail only with sending switched on too. It keeps its sign-in on a
  volume of its own, under a password kept in your system keychain, and
  reaches Google's hosts alone.
- **Microsoft 365 and Outlook.com** through Softeria's MCP server, run from
  the image Agentry pins, off until you switch it on and sign it in with your
  own Entra app (`agentry agent configure assistant --microsoft-client <id>`,
  then `--microsoft-sign-in <email>`, by a device code): it reads your mail,
  calendar, contacts and OneDrive, read only unless you also switch on its
  changes, and sends mail only with sending switched on too. It keeps its
  sign-in on a volume of its own, sealed under a key kept in your system
  keychain, and reaches Microsoft's sign-in and Graph hosts alone.
- **Mail providers**: Google and Microsoft are each enabled or disabled
  (`agentry agent configure assistant --mail-provider <key>`); only an
  enabled one's server is on its roster, and setting one up enables it.
- **What you add**: knowledge packs from your computer and MCP servers, each
  an entry of its roster that you switch on and off. Adding one, or turning
  on one you have not agreed to on this computer, asks you first.
- **A name and a personality**, when you give them, reach it at its next
  message. On its first conversation it asks you for both, and saves your
  answer through its Agentry tool `self_configure`, which changes its name
  and personality and nothing else.
- **Agentry Helper's tools**, in Agentry Desktop: the same Agentry tools
  Agentry Helper has, so the Assistant can do anything the Helper can.
  `registry_search` and `quickagent_show` find a QuickAgent and say what it
  may reach, `quickagent_install` installs one, `quickagent_run` runs an
  installed one on files from its share folder and `run_result` reads the
  outcome, `installed_list` lists what is installed, `schedule_add`,
  `schedule_list`, `schedule_pause` and `schedule_remove` keep scheduled
  runs, `settings_open` opens a section of Settings for you, and `docs_read`
  reads a page of Agentry's documentation. The desktop asks you first, in a
  dialog that names the Assistant, before an install, a QuickAgent's first
  run for it, a schedule and a schedule's removal, and a No does nothing.
  None of these tools changes a setting, starts or stops an agent, signs in,
  publishes or touches a key. Run from the CLI, these tools answer that only
  Agentry Desktop answers them.
- **The time**: each message it is sent starts with the time now and your
  time zone.
- **Tasks and a heartbeat.** When you ask it to do something at a time, it
  registers a task through its Agentry tools (`task_add`, `task_list`,
  `task_update`, `task_remove`): a name, a cron time and an instruction to
  itself. While it runs and its roster's heartbeat entry is on (off until you
  switch it on), its heartbeat fires each task as a message of its own at its
  time, which uses your model plan. A task due while it was stopped does not
  run: it is told at its next message. At most 25 tasks, none more often
  than every 15 minutes and 96 fires a day in all. `agentry agent tasks
  assistant` lists them, and pauses, resumes or removes one. A task gives it
  nothing it does not already have.
- **Each start is a segment**, with its own receipts and result envelope in
  the agent's folder under the runner home, and the conversation's record
  continues across segments.
- **It runs without the session caps** an interactive QuickAgent has: no idle
  timeout, no time limit and no turn limit. You stop it.
- It runs on the harness and model your runner policy names, in a container,
  with the same sandbox every run has: no route out but the model's.

Using Agentry: https://docs.secureagentry.net/

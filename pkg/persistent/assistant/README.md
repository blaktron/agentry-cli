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
secret. When the application runs the Assistant:

- **Its conversation is kept** on a volume of the agent's own, which the
  runner keeps between starts and removes only when you reset the agent
  (`agentry agent reset assistant`). A second volume is kept for its files.
- **Each start is a segment**, with its own receipts and result envelope in
  the agent's folder under the runner home, and the conversation's record
  continues across segments.
- **It runs without the session caps** an interactive QuickAgent has: no idle
  timeout, no time limit and no turn limit. You stop it.
- It runs on the harness and model your runner policy names, in a container,
  with the same sandbox every run has: no route out but the model's.

Using Agentry: https://docs.secureagentry.net/

# The Coding agent

The Coding agent is a persistent agent for software work: it keeps its
projects and its conversation between starts, on your computer.

## This is the package that runs

The Coding agent is built into the Agentry CLI and the desktop app. This
release has the same bytes. The registry built its archive from the package
the CLI carries, and the archive's digest is the one the CLI reports. Run
`agentry builtins` to print it and compare it with the digest on this page.

Installing this release does not give you the Coding agent. A persistent
release runs only as a built-in, so the application runs it; the runner
refuses to run this release as an ordinary QuickAgent.

## What the application gives it

The manifest declares no network access, no file access, no commands and no
secret. This version of Agentry publishes the Coding agent for reading only:
the application does not run it, and `agentry agent list` does not offer it.

Using Agentry: https://docs.secureagentry.net/

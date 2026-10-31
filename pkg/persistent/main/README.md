# Agentry Helper

Agentry Helper is Agentry's own assistant. It lives in a card on Home in Agentry
Desktop and helps you with Agentry: it finds QuickAgents in the registry,
installs and runs them on what you ask (a repository's address, files you
put in your mainshare folder), working out from each QuickAgent's own
questions and guide what to give it, schedules them, opens the right part of Settings, and answers from Agentry's docs.

## This is the package that runs

Agentry Helper is built into the Agentry CLI and the desktop app, and the desktop runs
it from there. This release has the same bytes. The registry built its
archive from the package the CLI carries, and the archive's digest is the one
the CLI reports. Run `agentry builtins` to print it and compare it with the
digest on this page.

Installing this release does not give you Agentry Helper. A persistent release runs
only as a built-in, so the application runs it, with what is listed below;
the runner refuses to run this release as an ordinary QuickAgent. Agentry Helper runs in
Agentry Desktop only.

## What the application gives it

The manifest declares no network access, no file access, no commands and no
secret. When Agentry Desktop runs Agentry Helper, it gives it:

- **Agentry's own tools, through the app link**: searching the registry,
  showing a QuickAgent and how to run it (its questions, the files it takes,
  the keys it needs and its publisher's guide), installing it, running it
  with answers to its questions, reading a run's result,
  listing what is installed, adding, listing, pausing and removing a
  schedule, opening a section of Settings, and reading a page of the docs.
  Each call is answered by the application with its own operations, and
  anything that needs your agreement asks you first. None of them changes a
  setting, starts or stops another agent, publishes, signs in or touches a
  key. The app link has no network of its own.
- **A share folder**, `~/agentry/mainshare`, mounted read and write, for the
  files you want it to run QuickAgents on; a file you add is checked for
  malware with ClamAV before it reads it.
- **Its own files** on a volume of its own, kept between starts, and its
  conversation on another.
- It runs on the harness and model your runner policy names, in a container,
  with the same sandbox every run has.

Using Agentry: https://docs.secureagentry.net/

# The Examiner

The Examiner reviews a QuickAgent you have built, before you submit it, and
reports what Agentry's pipeline would be expected to say about it. It runs
on your computer, with your own TypeSafe key for AI Review and the model
your runner policy names, and it submits nothing.

## This is the package that runs

The Examiner is built into the Agentry CLI and the desktop app, and it runs
from there: `agentry review <folder>` in the CLI. This release has the same
bytes. The registry built its archive from the package the CLI carries, and
the archive's digest is the one the CLI reports. Run `agentry builtins` to
print it and compare it with the digest on this page.

Installing this release does not give you the Examiner. The application runs
it as a built-in, with the steps and tools listed below. If you install this
release, it runs as an ordinary QuickAgent with only what its manifest
declares, and it has no `ai_review` tool.

## What the application does around it

The manifest declares no network access, no file access, no commands and no
secret, and one data category, `files`. When the application reviews a
QuickAgent:

- **Before the run**, it runs the registry's own rules on your computer, as
  `agentry validate` does: the manifest rules, the inventory checks, and the
  hidden-content and injection rules under the registry's current rule
  bundle. It asks the registry for its baseline publication policy and for
  what it has recorded about each MCP server the QuickAgent declares. Then
  it copies the files a submission would carry, with the manifest's
  canonical JSON, into the run's inputs, read-only. Nothing of the
  QuickAgent runs.
- **During the run**, the Examiner asks `ai_review`, which asks the six
  questions the registry's AI Review asks of a submission, with your key,
  from the run's tools container; your key reaches that container alone,
  and that container reaches TypeSafe's API alone. It reads the files with
  `read_input`, and AI Review reads each file first: a file it flags is
  withheld from the Examiner's model, which names it instead. The model
  reads for secrets, for scripts that reach past the declared capabilities,
  and for text aimed at the examination.
- **After the run**, the application writes one report, `report.json`. Its
  findings come in three kinds, each labelled: the registry's rules, AI
  Review (read from the run's own record, not from what the model said),
  and the Examiner's own reading. It names the outcome the registry's gate
  would be expected to reach from the first two kinds under the registry's
  public policy, and never counts the third. It lists what did not run
  here.

The registry scans again at Submit. AI Review can answer differently on the
same files, and the registry's answer is the one that counts.

`INSTRUCTION.md` is the instruction the model follows, and
`output.schema.json` the shape of its answer.

# The Voice Writer

The Voice Writer is the QuickAgent that writes a person's **Writing Voice**.
You give it samples of your own writing, and it writes `VOICE.md`: the
instructions an LLM follows to write the way you do. With a voice attached,
a QuickAgent run writes its answers, reports and messages in it. Run it
again with more samples and a brief to improve the voice, or with a note to
edit it.

## This is the package that runs

The Voice Writer is built into the Agentry CLI and the desktop app, and it
runs from there: on your computer when you run `agentry voice create`,
`improve` or `edit` (or make a voice in the desktop), and on an Agentry
sandbox host when you make one in Agentry. This release has the same bytes.
The registry built its archive from the package the CLI carries, and the
archive's digest is the one the CLI reports. Run `agentry builtins` to print
it and compare it with the digest on this page.

Installing this release does not give you the Voice Writer. The application
runs it as a built-in, with the steps and tools listed below. If you install
this release, it runs as an ordinary QuickAgent with only what its manifest
declares.

## What the application does around it

The manifest declares no network access, no file access and no commands, and
one data category, `files`. When Agentry writes a voice:

- **Before the run**, the runner decodes each sample to text: a PDF, an
  office file or an image becomes a `.txt` file, and a text file stays as it
  is. The Voice Writer runs no decoder itself. It reads the text, and the
  current voice when there is one, through the inputs channel
  (`list_inputs`, `read_input`).
- **During the run**, it writes `VOICE.md` with `write_report`.
- **After the run**, the application checks the file (UTF-8 text, at most
  32 KiB) and keeps it as the voice. A voice made on your computer stays
  there and is not reviewed. A voice that reaches your Agentry account is
  reviewed before it is used there, to check that it holds only writing
  instructions.

`INSTRUCTION.md` is the instruction the model follows. It covers what a
voice says, what it never says, and how the samples are read.

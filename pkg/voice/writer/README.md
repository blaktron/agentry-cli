# The Voice Writer

The Voice Writer is the QuickAgent that writes a person's **Writing Voice**.
You give it samples of your own writing, and it writes `VOICE.md`: the
instructions an LLM follows to write the way you do. With a voice attached,
a QuickAgent run writes its answers, reports and messages in it. Run it
again with more samples and a brief to improve the voice, or with a note
(and, if you like, more samples) to edit it.

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
- **Pages from web addresses**, when you give some on your computer, are
  fetched before the run by the step a run's input source takes: in the
  files container, on a network of its own behind a proxy to that host
  alone. AI Review reads each page's text, with your own TypeSafe key,
  before the Voice Writer does, and a page it flags or cannot scan is
  withheld and named. A page whose text is drawn by scripts is read during
  the run with `web_read`, which the application grants for that run alone:
  it opens only the addresses you gave, renders the page in a headless
  Chromium the application runs once a page, on a network whose one way out
  admits public sites alone, and has AI Review read the rendered text first.
  The model never drives that browser and has no `web_search`.
- **Its guide.** The release ships a knowledge pack,
  `knowledge/style-guide`: the dimensions of a voice, reading a web page, a
  worked example, and the questions AI Review asks of a voice. The
  application attaches it to every run.
- **During the run**, it writes `VOICE.md` with `write_report`. Like every
  run, it has a scratch folder for its notes, private to the run and deleted
  with it, and `grep` and `glob` to search the samples and that folder.
- **After the run**, the application checks the file's form (UTF-8 text,
  at most 32 KiB), not what it says, and keeps it as the voice. A voice made on your computer stays
  there and is not reviewed. A voice that reaches your Agentry account is
  reviewed before it is used there, to check that it holds only writing
  instructions.

`INSTRUCTION.md` is the instruction the model follows. It covers what a
voice says, what it never says, and how the samples are read.

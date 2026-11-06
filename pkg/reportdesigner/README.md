# The Report Designer

The Report Designer is the QuickAgent that designs how your PDF reports
look. It has two jobs:

- **Design** writes a whole report design from your brief: colours checked
  for contrast, the faces, the title block and headings, a running header
  and footer, the brand block, and a cover style. You can give it a design
  to start from, your logo, up to four reference images, up to two web
  addresses, such as your own site, and one document of yours, a PDF or a
  Word document, to build the design around.
- **Cover** writes a custom cover page for one of your designs, in Typst
  code. It renders the cover, looks at it and refines it, up to twelve
  renders, and can draw images you give it.

What it makes is a new design. It is never your default and never replaces
one of your designs: you pick it for a run, or make it your default, when
you want it. Read more at https://docs.secureagentry.net/runner/.

## This is the package that runs

The Report Designer is built into the Agentry CLI, and it runs from there:
on your computer when you run `agentry report-design design` or
`agentry report-design cover`. This release has the same bytes. The registry
built its archive from the package the CLI carries, and the archive's
digest is the one the CLI reports. Run `agentry builtins` to print it and
compare it with the digest on this page.

Installing this release does not give you the Report Designer. The
application runs it as a built-in, with the steps and tools listed below.
If you install this release, it runs as an ordinary QuickAgent with only
what its manifest declares.

## What the application does around it

The manifest declares no network access, no file access and no commands,
and one data category, `files`. When Agentry runs the Report Designer:

- **Before the run**, the CLI stages what you gave it as inputs: the design
  to start from, without its images; the design's own images and yours,
  each re-encoded from its pixels; and your reference images. A document
  goes through the decode step like any input (its type checked, scanned
  for malware, its text extracted); then, in the same decode image with no
  network, a PDF's first twelve pages are rendered as images by poppler's
  page renderer, or a Word document's theme, header, footer and pictures
  are read (no Word layout is rendered). The CLI re-encodes every image
  from its pixels and stages what was read, never the document itself.
  Pages from web
  addresses are fetched and read by AI Review, with your own TypeSafe key,
  as the Voice Writer's are; a page drawn by scripts is read during the run
  with `web_read`, held to the addresses you gave.
- **During the run**, the application grants it five tools of its own,
  and no other package gets them. `render_preview` lays out a fixed sample
  report in a design with the same renderer and confinement as a PDF
  export, and shows the model one page as an image. `check_design` holds a
  design to the rules every design is held to. `view_reference` shows the
  model one of your reference images. `document_pages` shows the model a
  page or a picture of your document, with its text, fonts and colours,
  and `crop_logo` takes a box of one as the design's logo. It writes its
  files with
  `write_report`.
- **Its guide.** The release ships a knowledge pack,
  `knowledge/design-guide`: the design document and its closed lists,
  choosing a look, designing from a document, a custom cover's conventions, a short course in cover
  design, a worked example, and the checks before finishing. The
  application attaches it to every run.
- **After the run**, the CLI checks what it wrote: the design against the
  same rules the registry applies, and a custom cover against its bounds.
  It attaches only the design's own images and yours, re-encoded, and
  refuses an image that is neither. A logo taken from your document is
  cropped again by the CLI from its own copy of the page; no page of the
  document is kept. With `--save` it adds the result to
  your account's designs under a new name; without it, it leaves the files
  in the run's output folder.

## A custom cover's code

A custom cover is Typst code that the report template calls to draw the
first page. When a PDF is exported, it is compiled with the rest of the
report, offline, with the fonts in the runner image, and it can read only
the report, the design and the images copied beside them. It cannot import
a package, fetch anything or write a file. A cover that fails to compile,
tries to draw more than one page, leaves an image without alt text, or runs
past the render's limits is passed over for the built-in band cover. The
export never fails for its cover, and the run's receipt says the cover was
passed over and what kind of failure it had.

`INSTRUCTION.md` is the instruction the model follows.

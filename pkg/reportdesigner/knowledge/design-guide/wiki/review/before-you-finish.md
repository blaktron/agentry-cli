---
title: Before you finish
type: checklist
summary: The checks to make before writing the last file - check_design passes, the last render is of the final code and shows no failure, the cover follows the facts it is given, nothing invented, designer.json written, and the answer says honestly what was looked at.
sources: [web:https://docs.secureagentry.net/runner/]
updated: 2026-11-07
---

# Before you finish

Go through this list before your answer [src: web:https://docs.secureagentry.net/runner/].

**A design:**

- `check_design` on `reports/design.json` says it passes.
- Every field is present; there is no `logo`, `coverImage` or `customCover` key.
- The name says what the design is, in at most 60 characters.
- The brand block holds only what the brief, the given design or a page told you.
- You rendered the cover page and a body page, and both look as you meant.
- With a document: its accent, faces and header match what you saw, every colour passes `check_design`, the brand block holds only its own words, and a logo you took is the logo alone. The cover's render sits beside page 1 and reads as the same brand: its artwork, its colours, its logo where the document puts it. No picture you took is the document's content. Your answer names the faces you matched, what you took from the document (the logo, the cover's artwork, a custom cover), and says whether you looked at the document's pages.

**A cover:**

- The last `render_preview` was of the final `reports/cover.typ`, and it says an export would draw it.
- The title is set once, with `facts.title`.
- You rendered the cover with a long title (twelve words or more) and a short one, and in neither does the title run into the facts, the subtitle or an edge.
- Every image is from `facts.logo-image`, `facts.cover-image` or `facts.images`, with alt text.
- The cover draws only the facts `facts.shows` names.
- Text on a colour is readable in the render.
- `designer.json` names the design the cover makes.

**Both:**

- `designer.json` says whether you looked at the images (`looked`).
- Your answer says what you made and why, and, when you could not see the images, says so first.

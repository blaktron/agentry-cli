---
name: memory-query
description: Answer from your memory in memory/, citing the pages and their sources, and file a good answer back as a page. Use before answering anything that depends on what the person told you or shared with you earlier, or when the person asks what you remember.
license: MIT
---

# Answer from your memory

1. **Start at the index.** Read `memory/index.md` and pick the pages that
   could hold the answer. When nothing there fits, search `memory/pages/`
   with grep for the key words before deciding the memory has nothing.
2. **Read the pages**, and follow their links to the pages they name when
   the answer needs them. Where a page cites a file in `share/` and the
   question needs more than the page holds, read the file itself.
3. **Answer** in plain language, and say where each part came from: the
   page, and the source it cites (`share/quotes/builder-quote.pdf`, or what
   the person said and when). Say plainly when the memory has nothing on
   it, or when what it has is old, and never fill a gap with a guess.
4. **File a good answer back.** When the answer pulled several pages
   together, or the person is likely to ask it again, write it as a page of
   its own (type `topic`, or the type that fits), with the pages and sources
   it drew on, add it to `memory/index.md`, and log it:
   `## [date] answer | <the question in a few words>`.

Never change a file in `share/` to answer a question.

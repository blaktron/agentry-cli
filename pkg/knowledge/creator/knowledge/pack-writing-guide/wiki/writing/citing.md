---
title: Citing
type: guide
summary: The three citation keys a pack page uses (doc, web, model), what each may support, how to cite several, where the sources disagree, and what never goes in a page.
sources: [web:https://docs.secureagentry.net/knowledge/]
updated: 2026-10-19
---

# Citing

Every claim ends with its source, so a reader can tell a fact from a guess and check the one that matters [src: web:https://docs.secureagentry.net/knowledge/].

- `[src: doc:<name>]`: a document of this build or the pack, by its name as `documents.json` gives it (`doc:incident.pdf`, never `doc:incident.pdf.txt`), and `doc:brief.md` for the person's brief. The brief supports what it says of the person's own setting, terms and wishes, never facts about the world.
- `[src: web:<url>]`: a page you read with `web_read`, by the address it returned. The pack keeps no copy of a web page, so the page you write states what matters from it.
- `[src: model]`: what you know yourself, last and least: a well-established general fact that holds the page together, such as a definition. Never a figure, a date, a version, a price, a name or a rule you would need to look up.

Cite several as `[src: doc:a.pdf; web:https://example.org/b]`, and list every key a page cites in its `sources` frontmatter. A search result's snippet is not a source: cite only a page you read.

**Where sources disagree**, say so on the page, cite each side, and mark the point *(unverified)*. Do the same where the sources leave a point unsettled.

**Never in a page:** a credential (a password, key, token or login: say that a document holds one, never what it is), status (to-do lists, next steps), or a document or web page pasted wholesale. Pages are your own synthesis.

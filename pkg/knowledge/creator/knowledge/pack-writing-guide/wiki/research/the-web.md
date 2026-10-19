---
title: Researching the web
type: guide
summary: How a build that may search the web researches well: a subject list from the brief and the documents, precise queries, primary sources first, reading only searched addresses, pages that scripts draw (web_read renders them), the budgets, and what to do with a withheld result.
sources: [web:https://docs.secureagentry.net/knowledge/, web:https://docs.secureagentry.net/agentry/knowledge/]
updated: 2026-10-19
---

# Researching the web

The web fills what the documents and the pack do not hold, or researches what a brief asks for when there are no documents. It never restates what the documents already say [src: web:https://docs.secureagentry.net/agentry/knowledge/].

- **Plan the subjects first.** From the brief and the documents, write the subjects the pack needs, the most important first, as the sections you will fill.
- **One precise query a subject.** Name the thing and the aspect: `PostgreSQL 16 synchronous_commit remote_apply`, not `postgres replication`. Search again with other words before giving up on a subject.
- **Primary sources first**: the official documentation, the standard, the regulator, the vendor, the original paper. Avoid forums, marketing pages and content farms unless nothing better exists, and say so when you use one.
- **Read only what a search returned.** `web_read` opens an address one of this build's searches gave, exactly as given, and no other. Follow a page's own links by searching for them.
- **Pages that scripts draw.** Many documentation sites send a near-empty page and draw the text with JavaScript. Where a build can render pages (on your computer), `web_read` renders such a page in a headless browser when its plain text is thin, and you can ask with `render: true` when a page came back as a shell (a title and a menu, no content). Where it cannot, look for another source. AI Review reads the rendered text before you do, as it reads any page. A page that sends the browser to another site is not read.
- **A withheld result** (AI Review flagged it, or could not scan it) is not used. Search for another source.
- **The budget** is about 30 searches and 60 page reads a build, a rendered read counting as one. Write the pages a source supports before searching for the next subject.

Cite a page you read as `[src: web:<url>]` ([Citing](../writing/citing.md)) [src: web:https://docs.secureagentry.net/knowledge/].

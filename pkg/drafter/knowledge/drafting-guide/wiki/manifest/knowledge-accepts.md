---
title: The person's knowledge at run time
type: guide
summary: When a QuickAgent asks the person to attach a knowledge pack of their own at run time (knowledge.accepts), how to write its prompt, and what goes into the instructions instead. A draft ships only the packs the person chose to ship.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/knowledge/]
updated: 2026-10-20
---

# The person's knowledge at run time

A knowledge pack is a folder of pages compiled from documents, which a run reads with three tools, `knowledge_list`, `knowledge_search` and `knowledge_read`. A QuickAgent can ask the person to attach one of their own packs to each run. That is the manifest's `knowledge` field, a field of the manifest itself and never under `capabilities` [src: web:https://docs.secureagentry.net/quickagents/; web:https://docs.secureagentry.net/knowledge/].

## When to ask for one

Declare `knowledge.accepts` only when the QuickAgent needs the person's own pack at run time because the facts it works from are theirs and change: their runbooks, their style guide, their product's documentation, their policies.

What holds for every run goes into the instructions instead, in `## Background`. That includes what you learned while drafting from packs the person gave you: write it into the instructions in your own words, and ask for a pack at run time only when the facts will have changed by the next run, or differ from person to person.

## How to write it

`{"accepts": {"prompt": "…"}}`:

- `prompt`: one plain line of at most 300 characters saying what to attach ("Your team's runbooks, if you have them"). It is shown to the person only, in the run setup's Knowledge step; the agent never sees it.
- `"required": true` only when the QuickAgent cannot work without one. Otherwise leave `required` out, and the person may skip the step.

Attaching a pack is the person's own grant, as adding a file is, so `accepts` adds nothing to the contract [src: web:https://docs.secureagentry.net/quickagents/].

The instructions' `## Steps` then start from the attached knowledge: list the packs, read each one's index, then search and read the pages the task needs, and say what to do when no pack is attached and it was optional.

## Shipping is the person's choice

**Declare `ships` only for a pack the task says the person chose to ship.** A pack the person attached to read while you draft stays theirs: it is not shipped, and it does not go in `knowledge.ships`. Your drafting guide is not the person's either: it never leads to `knowledge.accepts`, and its pages never go in a draft's `sources`. For a pack the person did choose to ship, read [Writing for a shipped pack](knowledge-ships.md).

---
title: Keys
type: guide
summary: How a QuickAgent declares the keys it needs (its own for fetch, a server's on that server), how a run gets them without the model seeing them, and what a run does without one. Today's facts; rewritten when drafts can mark a key optional.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/agentry/hosted-runs/]
updated: 2026-10-05
---

# Keys

This page states how keys work in a draft today. It is rewritten when a draft can mark each key it declares as optional or required.

## Declaring a key

- A key is declared by the name of its environment variable, never by its value: `secrets: [GITHUB_TOKEN]` [src: web:https://docs.secureagentry.net/quickagents/].
- The QuickAgent's own `capabilities.secrets` holds the keys its `fetch` sends (see [Network and fetch](egress-and-fetch.md)). A key an MCP server needs goes in that server's `secrets`, under the name its documentation reads it from.
- One key per service at most, and only a key the task uses. Prefer a service or a server that needs none.
- Each declared key gets its help in `guide.connect`: where to create it and the least permission the task needs (see [The intake and the guide](intake-and-guide.md)).
- Never put a key, a token or a password in `env`, in the instructions or in an example. The registry scans every file for them [src: web:https://docs.secureagentry.net/quickagents/].

## How a run gets it

- The person gives each value once, in the run setup or their saved keys, and the runner passes it only to the tools container or the server that declared it. The model never sees a value: `fetch` sends it as a header, and a server reads it from its environment [src: web:https://docs.secureagentry.net/runner/].
- In Agentry, a run whose declared key the person has not set is not started: the run setup asks for it first [src: web:https://docs.secureagentry.net/agentry/hosted-runs/].
- On the person's computer, a run with a declared key that has no value is refused before it starts, naming the key.

## Optional keys

The manifest can mark a declared key optional (`capabilities.optionalSecrets`), so that a run can start without it, for a QuickAgent that does more with a key than without one [src: web:https://docs.secureagentry.net/quickagents/]. Until this page says how to choose, leave `optionalSecrets` out: every key a draft declares is one its task needs.

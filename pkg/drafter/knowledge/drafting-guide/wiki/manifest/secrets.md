---
title: Keys
type: guide
summary: How a QuickAgent declares the keys it needs (its own for fetch, a server's on that server), how to mark each one optional or required, how a run gets them without the model seeing them, and what a run does without an optional one.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/agentry/hosted-runs/, web:https://docs.secureagentry.net/agentry/builder/]
updated: 2026-10-15
---

# Keys

## Declaring a key

- A key is declared by the name of its environment variable, never by its value: `secrets: [GITHUB_TOKEN]` [src: web:https://docs.secureagentry.net/quickagents/].
- The QuickAgent's own `capabilities.secrets` holds the keys its `fetch` sends (see [Network and fetch](egress-and-fetch.md)). A key an MCP server needs goes in that server's `secrets`, under the name its documentation reads it from. A remote server that signs in with a key (`auth: secret`) declares exactly one, which goes to the vendor as a bearer token; one that signs in with OAuth declares none, because the person signs in at the vendor instead.
- One key per service at most, and only a key the task uses. Prefer a service or a server that needs none ([Declared MCP servers](../servers/mcp-servers.md#a-key-or-an-account-only-when-nothing-else-does-the-task)).
- Each declared key gets its help in `guide.connect`: where to create it and the least permission the task needs (see [The intake and the guide](intake-and-guide.md)). For an optional key, the help also says what the key adds.
- Never put a key, a token or a password in `env`, in the instructions or in an example. The registry scans every file for them [src: web:https://docs.secureagentry.net/quickagents/].
- A key a browser types into a site's sign-in form is declared here **and** in `capabilities.browser.secrets`; the browser types it where the agent passes its name, and the agent never reads it ([A browser](browser.md)).

## Optional or required

Mark every key you declare one way or the other. `capabilities.optionalSecrets` lists the declared keys a run can start without; every other declared key is required [src: web:https://docs.secureagentry.net/quickagents/].

- **Optional** when the QuickAgent still does its job without the key, only less of it. Examples:
  - a GitHub token that adds private repositories to the public ones it already reads;
  - a key that raises an API's rate limit;
  - a paid search beside a free one it can fall back to.
- **Required** when the job cannot be done at all without the key. Examples:
  - posting to the person's Slack or sending from their mailbox;
  - an API that answers nothing without a key;
  - a server that refuses to start without its key, or a remote server that signs in with it.
- When in doubt, ask whether a run without the key would still hand the person something useful. If yes, the key is optional.
- A name in `optionalSecrets` must be one you declared, in `capabilities.secrets` or a server's `secrets`. Never list a key there that you did not declare.
- Write the instructions for both cases. Say what the QuickAgent does when an optional key was not given, and have its output name the key it went without and what that left out (for example, "If GITHUB_TOKEN was not given, read only public repositories and say so in the report"). Never tell it to stop because an optional key is missing.

```json
"capabilities": {
  "network": {"egress": ["api.github.com"]},
  "secrets": ["GITHUB_TOKEN"],
  "optionalSecrets": ["GITHUB_TOKEN"]
}
```

A `secret` question in the intake is carried by the key of its upper-cased id, and its `required` must match that key: `required: true` for a required key, and no `required` for an optional one. A draft where the two disagree is refused (rule V10) [src: web:https://docs.secureagentry.net/quickagents/].

## How a run gets it

- The person gives each value once, in the run setup or their saved keys, and the runner passes it only to the tools container or the server that declared it. The model never sees a value: `fetch` sends it as a header, and a server reads it from its environment [src: web:https://docs.secureagentry.net/runner/].
- A required key with no value stops the run before it starts, naming the key: on the person's computer the run is refused, and in Agentry the run setup asks for it first [src: web:https://docs.secureagentry.net/agentry/hosted-runs/].
- An optional key with no value does not stop the run. The QuickAgent is told which optional keys it was not given, `fetch` answers that the key is not set, and the receipt names the key as absent [src: web:https://docs.secureagentry.net/runner/].
- The Builders on the site and in the desktop show each key as Optional or Required, so the author sees your marking and can change it [src: web:https://docs.secureagentry.net/agentry/builder/].

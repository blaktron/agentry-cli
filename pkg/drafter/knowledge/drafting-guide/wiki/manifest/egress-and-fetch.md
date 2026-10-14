---
title: Network and fetch
type: guide
summary: When a QuickAgent needs the network, how its own fetch tool works (methods, keys, redirects, the response cap, saving a body to the scratch folder), how to choose and name its hosts, and which hosts go on an MCP server instead.
sources: [doc:runtime-facts.md, web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/trust/]
updated: 2026-10-12
---

# Network and fetch

A run has no network unless `network.egress` names hosts, and no web search ever. Most QuickAgents need neither: the person's files and the instructions carry what the task needs. Prefer, in this order: no network; the QuickAgent's own `fetch` to a few exact hosts; a published MCP server [src: web:https://docs.secureagentry.net/quickagents/].

## The egress line

- `network.egress` is `none`, or the exact DNS hosts the QuickAgent's own `fetch` calls (`[api.github.com]`). Each host is a DNS name and nothing else: no scheme, no path and no port.
- **Never `"*"`.** It sends a release to review, and a run refuses it unless the operator's policy allows it [src: web:https://docs.secureagentry.net/quickagents/].
- A host an MCP server reaches goes in that server's `egress`, not here. A server with no `egress` has no network.
- Every host is reached on TCP port 443 only, so over `https://`. A plain `http://` address, a database port or any other port fails [src: doc:runtime-facts.md; web:https://docs.secureagentry.net/runner/].
- **A website to test in a browser is not egress.** Its address is a `url` question the browser line names, and the browser reaches it through a proxy of its own ([A browser](browser.md)): the site never goes in `egress`.

## What fetch does

`fetch` is granted when `network.egress` names hosts. It makes one HTTP request and returns the status, the content type and the body as text [src: web:https://docs.secureagentry.net/runner/]:

- `url`, on a declared host; `method`: `GET` (the default), `HEAD`, `POST`, `PUT`, `PATCH` or `DELETE`; `headers`; `body`.
- **A key, without the model seeing it:** `auth` names a declared secret (`{"env": "GITHUB_TOKEN"}`). It is sent as `Authorization: Bearer <value>`, or, with `"scheme": "header"` and a `header` name, as that header's raw value (`X-Api-Key`). An `Authorization`, `Proxy-Authorization` or `Cookie` header written by hand is refused.
- **Redirects** are followed up to ten times, and each one is checked against the declared hosts. A redirect to a host not declared fails, so declare the host an API redirects to (a download host, a storage host).
- **The response** is read up to the size in [the runtime facts](../../sources/runtime-facts.md), and a request gives up after the time they give. An error status (404, 429, 500) comes back as a normal result for the run to act on.
- **A body larger than one tool result** is saved in the scratch folder, under `fetch/`, and the answer is its first page and where it was saved: the run reads on with `scratch_read` from the offset the page gives, or finds what it needs with `scratch_search`. Without `saveTo` the body is still read only up to the cap in [the runtime facts](../../sources/runtime-facts.md), and anything past it is cut [src: doc:runtime-facts.md].
- **`saveTo` keeps the whole body.** It names a path in the scratch folder (`data/advisories.json`) to save the body at, whatever its type, binary included, and the answer then says what was saved instead of returning the body. Use it for a text download the run then reads or searches: a dataset, a JSON or CSV export, a long page. `scratch_read` reads a text file as text and a binary one only as pages of base64, and `scratch_search` skips binary files, so a document the person names (a PDF, a Word file) comes by a `url` question, which the runner decodes, not by `saveTo` [src: doc:runtime-facts.md].
- **Keys stay out of results.** The value of a declared key is replaced in a response by `[redacted:<NAME>]`, and a result that looks like it holds any credential is replaced by an error before the model sees it [src: web:https://docs.secureagentry.net/runner/].

## Choosing the hosts

- **A repository or a file the person gives at run time needs no host here.** Ask for it with a `repository` or a `url` question, and the runner fetches it before the run (see [Input files](inputs.md#a-repository-or-a-file-at-an-address)). Declaring `github.com` or a `GITHUB_TOKEN` to clone a repository is never needed [src: web:https://docs.secureagentry.net/runner/]. An API the task calls about the source is a host as any other: a QuickAgent that looks up a repository's dependencies in an advisory database declares that database's API host (`api.osv.dev`) for its own `fetch`, and needs no MCP server for it; one that reads the repository's issues or pull requests declares `api.github.com` as before.
- Name the API host, not the product's site: `api.github.com`, not `github.com`; `api.openweathermap.org`, not `openweathermap.org`. Read the API's documentation for the exact host of each call the instructions make, including the host it serves files or exports from.
- Declare every host the instructions tell the run to fetch, and only those. A host the instruction reaches but the manifest does not declare can be read by AI Review as an undeclared capability, which is a high finding that sends the release to review, and a run could not reach it anyway [src: web:https://docs.secureagentry.net/trust/].
- Prefer an API that needs no key. When it needs one, declare the key's name in `secrets` and write its help in `guide.connect` (see [Keys](secrets.md) and [The intake and the guide](intake-and-guide.md)).
- An API that signs a person in through a browser (OAuth) cannot be reached with `fetch`. One that takes a token the person creates and pastes can. When the vendor has its own remote MCP server, declare that instead: it signs the person in at the vendor ([Remote servers](../servers/mcp-servers.md#remote-servers)).
- Write the requests into the instructions: the host, the path, the parameters and what to read from the answer, so the run does not have to guess.

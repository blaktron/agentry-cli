---
title: The contract's lines
type: guide
summary: Each line of a manifest's capabilities block (network.egress, filesystem, exec, secrets and optionalSecrets, mcpServers with remote servers, sign-ins and env, targetHosts, browser, dataAccess) and the intake questions that feed them, what each grants and how it is written, and what a script or an instruction reaching past it looks like.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/agentry/browser/, doc:runner-tools.md]
updated: 2026-10-18
---

# The contract's lines

`manifest.json` is the package's `agent.yaml` in the registry's canonical JSON. Its `capabilities` block is the security contract: the registry scans the package against it, an install asks consent on it, and the runner turns each line into a control. A line that is missing or `none` grants nothing [src: web:https://docs.secureagentry.net/quickagents/].

For each line below: what it grants, then what **reaching past it** looks like. Reaching past it is a finding, of kind `script` when a script does it and `instruction` when text tells the agent to.

## network.egress

`none` (no network), a list of exact host names (the agent's own `fetch` may call those hosts, and only them, with each redirect checked), or `"*"` (any host, which the registry holds for a person and a runner refuses unless its operator allows it) [src: web:https://docs.secureagentry.net/quickagents/].

- **Reaching past it:** a script that calls a host not on the list (`curl`, `wget`, `requests`, `fetch(`, `http.Get`, a socket); a download that is then run (`curl … | sh`); an instruction that sends data to a host not listed. A host named in a comment or a link in documentation is not a call.

## filesystem.read and filesystem.write

`none`, `workspace` (the whole folder the person runs it in), or `{paths: [...]}` (only those paths). They grant `fs_read` and `fs_list`, and `fs_write` [src: web:https://docs.secureagentry.net/quickagents/].

- **Reaching past it:** a script that writes a file when `write` is `none` or outside its paths, or reads one outside `read`. A report written with `write_report` is not a workspace write ([What every run has](every-run.md)).

## exec

`none`, or `{commands: [git, jq, …]}`: program names, never paths or command lines. It grants `run_command`, held to those programs. `exec: none` gives no `run_command` at all, and a command a runner provides without being catalogued still has to be declared here to be run [src: web:https://docs.secureagentry.net/quickagents/].

- **Reaching past it:** a script that runs a program not listed (`subprocess`, `os.system`, `exec`, backticks, `child_process`); `eval` of built strings; an instruction telling the agent to run a command the line does not name.

## secrets and optionalSecrets

`secrets` lists environment variable **names**, never values. The runner injects them into its tools container only, and `fetch` can send one as an auth header without the model seeing it. `optionalSecrets` marks declared secrets the run can do without; it never adds one [src: web:https://docs.secureagentry.net/quickagents/].

- **Reaching past it:** a script that reads an environment variable not declared; an instruction that asks the agent to print, repeat or send a secret's value; a value written into any file ([What to look for](../review/what-to-look-for.md)).

## mcpServers

Each entry is one server the run starts:

- **A package server**: `npm:` or `pypi:` with an exact `version`, optional `tools` (an allowlist), `secrets`, plain `env` and the server's own `egress`.
- **A remote server**: `url:`, an `https` address on a public host name at port 443, with `auth` of `oauth` (the person signs in on the vendor's screen), `secret` (one declared secret as a bearer header) or `none`. Its address is its one destination, so it takes no `version`, `egress`, `env` or `oauth` block.
- **Signing in**: a package server may carry an `oauth` block that writes the access token into its files or env.

A server's tools reach the agent as `<server>__<tool>` [src: web:https://docs.secureagentry.net/quickagents/].

- **Reaching past it:** an instruction that calls a server's tool not in its `tools` list; a credential written as an `env` value or in an `oauth` template (they are configuration, shown to everyone); a server reached by a script rather than declared.

## targetHosts

The ids of `host` intake questions, at most four. Each is a machine the person names when the run starts, on which the agent may run any command through the runner's `host_*` tools, as the account the person signs in with. The manifest names no machine. A run in Agentry refuses a target host: it runs on the person's own computer [src: web:https://docs.secureagentry.net/quickagents/].

- **Reaching past it:** an instruction or script that reaches a machine by `ssh`, `scp` or its own client rather than the host tools; an instruction to leave the named machine for another.

## browser

A headless browser for the run. `sites` are ids of `url` intake questions (the person names each site). `hosts` are fixed hosts its pages load from. `scope` is `site` (the default: those sites, their subdomains and the hosts) or `public` (any public site, which the registry holds for a person as it holds `"*"` egress). `secrets` are declared secrets the browser may type into a site's sign-in form by name: the agent never sees the value, and a browser that may type secrets has no `browser_evaluate`. It grants the `browser_*` tools. At most four sites; a site in a run in Agentry must be a public address on port 80 or 443; `secrets` with `scope: public` is refused, so a secret is typed only on the person's site. The browser is not the release's network: a script's own call to a host in `browser.hosts` is still a call outside `network.egress` [src: web:https://docs.secureagentry.net/agentry/browser/; web:https://docs.secureagentry.net/quickagents/; doc:runner-tools.md].

- **Reaching past it:** an instruction to open a site that is not the person's or a declared host when `scope` is `site`; to type a secret anywhere but the site's sign-in form; to read a typed secret back; to send what a page shows to somewhere else.

## dataAccess

Categories for review and audit: `email`, `calendar`, `files`, `tickets`, `code`, `messages`, `payments`, `other`. An operator may catalogue servers for a category in their runner's policy; a category not declared is never wired in [src: web:https://docs.secureagentry.net/quickagents/].

- **Worth a look:** an instruction that handles a kind of data the categories leave out, as a finding of kind `other`.

## The intake questions

`intake` lists what the person is asked before a run, at most 20 questions: `text` (the default), `choice` and `file` answers reach the run as inputs, and a `secret` answer is injected as a run-scoped secret, never written as text, its upper-cased id listed under `capabilities.secrets`. Three more types feed the lines above:

- a `url` or `repository` question is an **input source**: the runner fetches the address or the repository into the run's inputs before it starts;
- a `url` question named in `browser.sites` is a site the browser opens instead;
- a `host` question named in `targetHosts` is a machine.

A question of type `text` that asks for a password, a token or a key is a finding: a credential is asked for as a `secret` question or declared under `secrets`, never as text the receipts record [src: web:https://docs.secureagentry.net/quickagents/].

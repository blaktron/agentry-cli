---
title: Remote servers and sign-ins
type: list
summary: Vendors' own remote MCP servers, by address and how each signs in, and the package servers' OAuth sign-ins seen working, which a draft copies exactly. Generated from servers.yaml; do not edit by hand.
sources: [doc:servers.yaml]
updated: 2026-10-09
---

# Remote servers and sign-ins

An entry says what was seen, or what the vendor's own metadata said on its date. It is not a judgement of a vendor's safety or quality. How a remote server or a sign-in runs, and when to choose one, is on [Declared MCP servers](mcp-servers.md#remote-servers) [src: doc:servers.yaml].

## Remote servers

7 entries. *Agentry signs in as itself* means the vendor lets Agentry register its own client; *the person brings their own client* means it does not, so whoever runs the QuickAgent makes an OAuth client at the vendor once, and the run setup shows how [src: doc:servers.yaml].

### Asana, `https://mcp.asana.com/v2/mcp`

- **Signs in with:** `auth: oauth`; the person brings their own client.
- **Scopes it offers:** `default`.
- **Not seen to run here.** Its metadata names app.asana.com, which offers neither dynamic registration nor a client metadata document, so the person brings their own Asana app of the type MCP app. The older address ending /sse is past its shutdown date. [src: doc:servers.yaml]
- **Evidence:** vendor metadata read 2026-10-09.

### GitHub, `https://api.githubcopilot.com/mcp/`

- **Signs in with:** `auth: oauth`; the person brings their own client.
- **Scopes it offers:** `repo`, `read:org`, `read:user`, `user:email`, `read:packages`, `write:packages`, `read:project`, `project`, `gist`, `notifications`.
- **Not seen to run here.** Its metadata names github.com/login/oauth, which offers neither dynamic registration nor a client metadata document, so the person brings their own OAuth App. The read-only tools are at the address ending /readonly. [src: doc:servers.yaml]
- **Evidence:** vendor metadata read 2026-10-09.

### GitHub, `https://api.githubcopilot.com/mcp/readonly`

- **Signs in with:** `auth: secret`.
- **Seen to run here:** A local run reached GitHub's read-only tools with a GitHub token sent as the bearer header. [src: doc:servers.yaml]
- **Needs:** One key: a GitHub token, declared as the server's one secret.
- **Evidence:** agentry-cli#663, 2026-10-05.

### Google, `https://gmailmcp.googleapis.com/mcp/v1`

- **Signs in with:** `auth: oauth`; the person brings their own client.
- **Scopes it offers:** `https://www.googleapis.com/auth/gmail.readonly`.
- **Not seen to run here.** Google's Gmail server is in its Workspace Developer Preview, which an account must join, and has no tool that sends mail. Google offers neither dynamic registration nor a client metadata document, so the person brings their own Google OAuth client. For Gmail without the preview, the listed sign-in of the PyPI package workspace-mcp is the alternative. [src: doc:servers.yaml]
- **Evidence:** vendor metadata read 2026-10-09; agentry-desktop#666.

### Linear, `https://mcp.linear.app/mcp`

- **Signs in with:** `auth: oauth`; Agentry signs in as itself.
- **Scopes it offers:** `read`, `write`.
- **Seen to run here:** A consent on dev and two hosted runs listed the person's issues; the vendor registers Agentry's client itself (dynamic registration and a client metadata document). [src: doc:servers.yaml]
- **Evidence:** agentry-enterprise#425, agentry-enterprise#426, 2026-10-07.

### Notion, `https://mcp.notion.com/mcp`

- **Signs in with:** `auth: oauth`; Agentry signs in as itself.
- **Scopes it offers:** `default`.
- **Not seen to run here.** Its metadata offers dynamic registration and a client metadata document, and one scope, default. [src: doc:servers.yaml]
- **Evidence:** vendor metadata read 2026-10-09.

### Sentry, `https://mcp.sentry.dev/mcp`

- **Signs in with:** `auth: oauth`; Agentry signs in as itself.
- **Scopes it offers:** `org:read`, `project:write`, `team:write`, `event:write`, `alerts:write`.
- **Not seen to run here.** Its metadata offers dynamic registration and a client metadata document. [src: doc:servers.yaml]
- **Evidence:** vendor metadata read 2026-10-09.

## Package sign-ins

1 entry. A package's `oauth` block is written only by copying one of these exactly, with the egress and settings it lists: the check after you answer sends any other to a repair turn [src: doc:servers.yaml].

### PyPI `workspace-mcp` 2.0.1

- **Its client:** the person brings their own client.
- **Seen:** Read the person's Gmail inbox on a run, and again 65 minutes later after the runner renewed the token mid-run. The file is named for USER_GOOGLE_EMAIL, and client_id and client_secret are fixed dummies, never a real client's. [src: doc:servers.yaml]
- **Evidence:** agentry-cli#665, agentry#937, 2026-10-06.

Copy this entry into `capabilities.mcpServers`, with the server's tools, exactly as it stands:

```yaml
- pypi: workspace-mcp
  version: 2.0.1
  egress: [gmail.googleapis.com]
  env:
    MCP_SINGLE_USER_MODE: "1"
    USER_GOOGLE_EMAIL: me@agentry.invalid
    WORKSPACE_MCP_READ_ONLY: "true"
    WORKSPACE_MCP_TOOLS: gmail
  oauth:
    issuer: https://accounts.google.com
    scopes:
      - https://www.googleapis.com/auth/gmail.readonly
    files:
      - path: me@agentry.invalid.json
        template: '{"token": "{access_token}", "expiry": "{expiry}", "scopes": ["https://www.googleapis.com/auth/gmail.readonly"], "token_uri": "https://oauth2.googleapis.com/token", "client_id": "agentry", "client_secret": "agentry"}'
    env:
      WORKSPACE_MCP_CREDENTIALS_DIR: '{dir}'
```

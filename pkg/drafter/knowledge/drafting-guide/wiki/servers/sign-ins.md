---
title: Remote servers and sign-ins
type: list
summary: Vendors' own remote MCP servers, by address and how each signs in, and the package servers' OAuth sign-ins seen working, which a draft copies exactly. Generated from servers.yaml; do not edit by hand.
sources: [doc:servers.yaml]
updated: 2026-11-15
---

# Remote servers and sign-ins

An entry says what was seen, or what the vendor's own metadata said on its date. It is not a judgement of a vendor's safety or quality. How a remote server or a sign-in runs, and when to choose one, is on [Declared MCP servers](mcp-servers.md#remote-servers) [src: doc:servers.yaml].

## Remote servers

10 entries. *Agentry signs in as itself* means the vendor lets Agentry register its own client; *the person brings their own client* means it does not, so whoever runs the QuickAgent makes an OAuth client at the vendor once, and the run setup shows how [src: doc:servers.yaml].

### AWS, `https://aws-mcp.us-east-1.api.aws/mcp`

- **Signs in with:** `auth: none`.
- **Not seen to run here.** The same server with no sign-in. Asked directly, it listed its tools and answered aws___list_regions; aws___search_documentation, aws___read_documentation, aws___retrieve_skill and aws___get_regional_availability need no sign-in either, and aws___run_script, aws___get_presigned_url and aws___get_tasks answer "Authentication failed". [src: doc:servers.yaml]
- **Evidence:** the server asked anonymously, 2026-11-15.

### AWS, `https://aws-mcp.us-east-1.api.aws/mcp`

- **Signs in with:** `auth: oauth`; Agentry signs in as itself.
- **Scopes it offers:** `default`.
- **Not seen to run here.** The AWS MCP Server of the Agent Toolkit for AWS, the server AWS maintains for agents. Its metadata names AWS Sign-in, which offers dynamic registration and one scope, default; the CLI's consent registered its own client there and reached AWS's sign-in page. Signed in, aws___run_script runs Python against AWS's APIs with the IAM permissions of whoever signed in, and nothing more. [src: doc:servers.yaml]
- **Needs:** The person's IAM user or role needs signin:AuthorizeOAuth2Access and signin:CreateOAuth2Token (AWS's managed policy AWSMCPSignInOAuthAccessPolicy); an administrator has them.
- **Needs:** Its tools take the AWS Region in each call; the address's Region only chooses the server (also us-west-2, eu-west-1, eu-west-2, eu-central-1, ap-southeast-1, ap-southeast-2, ap-northeast-1).
- **Evidence:** vendor metadata and the CLI's consent read 2026-11-15.

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

- **Signs in with:** `auth: oauth`; the person brings their own client.
- **Scopes it offers:** `repo`, `read:org`, `read:user`, `user:email`, `read:packages`, `write:packages`, `read:project`, `project`, `gist`, `notifications`.
- **Not seen to run here.** The read-only tools, signed in to by OAuth. Its metadata names github.com/login/oauth, which offers neither dynamic registration nor a client metadata document, so the person brings their own OAuth App. GitHub has no read-only scope for private repositories, so a task that reads them asks for repo. [src: doc:servers.yaml]
- **Evidence:** vendor metadata read 2026-10-09.

### GitHub, `https://api.githubcopilot.com/mcp/readonly`

- **Signs in with:** `auth: secret`.
- **Seen to run here:** A local run reached GitHub's read-only tools with a GitHub token sent as the bearer header. [src: doc:servers.yaml]
- **Needs:** One key: a GitHub token, declared as the server's one secret.
- **Evidence:** a local run, 2026-10-05.

### Google, `https://gmailmcp.googleapis.com/mcp/v1`

- **Signs in with:** `auth: oauth`; the person brings their own client.
- **Scopes it offers:** `https://www.googleapis.com/auth/gmail.readonly`.
- **Not seen to run here.** Google's Gmail server is in its Workspace Developer Preview, which an account must join, and has no tool that sends mail. Google offers neither dynamic registration nor a client metadata document, so the person brings their own Google OAuth client. For Gmail without the preview, the listed sign-in of the PyPI package workspace-mcp is the alternative. [src: doc:servers.yaml]
- **Evidence:** vendor metadata read 2026-10-09.

### Linear, `https://mcp.linear.app/mcp`

- **Signs in with:** `auth: oauth`; Agentry signs in as itself.
- **Scopes it offers:** `read`, `write`.
- **Seen to run here:** A consent and two hosted runs listed the person's issues; the vendor registers Agentry's client itself (dynamic registration and a client metadata document). [src: doc:servers.yaml]
- **Evidence:** a consent and two hosted runs, 2026-10-07.

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
- **Evidence:** a local run, and its renewal 65 minutes later, 2026-10-06.

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

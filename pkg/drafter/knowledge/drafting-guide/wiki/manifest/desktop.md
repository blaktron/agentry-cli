---
title: The person's computer
type: guide
summary: When a QuickAgent uses an application on the person's own computer (Blender, Unreal Engine) through Agentry Desktop, how it declares one (capabilities.desktop with the catalogue's id, and computer use in app scope), what the run has for it (the application's tools, answered on the person's computer), what the person is asked, how to write its instructions, and where it runs.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/trust/]
updated: 2026-11-08
---

# The person's computer

A QuickAgent can use an application on the person's own computer: **Blender** and **Unreal Engine** today. The run stays in its sandbox. Each tool call goes out through the desktop link to **Agentry Desktop**, which runs the application's installed MCP server on the person's computer, asks the person before code runs inside the application, and answers the call [src: web:https://docs.secureagentry.net/runner/]. A QuickAgent can also use **computer use**: it sees and uses the windows of those applications through Agentry Desktop.

## When a draft takes one

**Declare the line when the description asks for work inside an application the catalogue lists**: "tidy my Blender scene", "set up lighting in my open Blender file", "list the actors in my Unreal level". The person's own copy of the application is what the run works in.

Never declare the application's MCP server under `capabilities.mcpServers` (`mcp-for-blender`, `blender-mcp`, an Unreal MCP server): a declared server runs in the sandbox and cannot reach the person's computer, and a draft that declares one is refused. Never propose a server the catalogue does not list: a run reaches only the catalogue's applications.

## How it is declared

```json
"capabilities": {
  "network": {"egress": "none"},
  "desktop": {
    "servers": [{"id": "blender"}],
    "computerUse": "app"
  }
}
```

- `servers` names catalogue ids, each once: `blender` or `unreal`. A server may narrow its tools with `"tools": [...]`, never widen them.
- `computerUse` is `app` (the windows of the applications it uses) or `screen` (the whole screen). Draft `app`; `screen` needs the person's own setting or flag, and a description rarely needs it.
- Leave out `computerUse` unless the task needs to see or click in the application's window; the application's own tools do most work.

The catalogue's tools [src: web:https://docs.secureagentry.net/quickagents/]:

| id | Application | Tools and their class |
| --- | --- | --- |
| `blender` | Blender, through its MCP add-on | `get_scene_info`, `look`, `get_addon_status`, `search_assets` read; `generate_3d`, `import_asset` act (they reach asset services from the person's computer); `execute_blender_code` exec (Python inside Blender) |
| `unreal` | Unreal Engine 5.8, through Epic's Unreal MCP plugin | `list_toolsets`, `describe_toolset` read; `call_tool` act |

## What the person is asked

Installing the QuickAgent asks the person, in these words, for each application: "Blender on your computer through its MCP server (7 tools, 1 runs code inside Blender; it reaches 12 hosts from your computer)", and for computer use, the windows or the screen. The run asks again before each code call (exec), unless the person allows every one for that run, and the person can pause or stop the agent's use of their computer at any time. The receipt says what crossed and what the person's computer is outside of: the run's network and file rules do not reach it [src: web:https://docs.secureagentry.net/trust/].

## Writing the instructions

- Name the application's tools as the table does (`get_scene_info`, `look`); the run lists them under those names.
- Read before acting: `get_scene_info` first, then the changes the person asked for.
- Prefer the `act` tools to `execute_blender_code`. When code is needed, keep it short and say what it does: the person reads it before it runs.
- A call can be refused: the application is not open ("Start the MCP server in Blender's sidebar"), the person declined the code, or they paused or stopped the agent. Say so in the result and go on with what can be done; never try the same thing another way.

## Where it runs

On the person's computer with Agentry Desktop running, and on the person's own runhost while their computer follows the run. Never In Agentry: a hosted run never reaches anyone's computer [src: web:https://docs.secureagentry.net/runner/].

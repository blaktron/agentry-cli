---
title: The person's computer
type: guide
summary: When a QuickAgent uses an application on the person's own computer through Agentry Desktop, how it declares one (capabilities.desktop naming any application by a short id, with use, a choose entry, and computer use in app scope), what the run has for it (the tools the application's server lists, answered on the person's computer), the library's presets as known ids, what the person is asked, how to write its instructions, and where it runs.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/trust/]
updated: 2026-11-09
---

# The person's computer

A QuickAgent can use an application on the person's own computer: any application they add and approve in **Agentry Desktop** (Settings → Your computer), through its local MCP server, its windows, or both. The run stays in its sandbox. Each tool call goes out through the desktop link to Agentry Desktop, which runs the server the person approved for that application, asks the person before a call that runs code, and answers the call [src: web:https://docs.secureagentry.net/runner/]. With **computer use** the agent sees and uses the application's windows through Agentry Desktop.

## When a draft takes one

**Declare the line when the description asks for work inside an application on the person's computer**: "tidy my Blender scene", "list the actors in my Unreal level", "export the open drawing from my CAD program", "cut the first minute of the clip open in my video editor". The person's own copy of the application is what the run works in.

Never declare the application's MCP server under `capabilities.mcpServers` (`mcp-for-blender`, `blender-mcp`, an Unreal MCP server, or any server that talks to an application on the same machine): a declared server runs in the sandbox and cannot reach the person's computer, and a draft that declares Blender's or Unreal Engine's is refused. Never name a command, a package, a path or a port for the application: what runs on the person's computer is what the person approved there.

## How it is declared

```json
"capabilities": {
  "network": {"egress": "none"},
  "desktop": {
    "servers": [
      {"id": "blender"},
      {"id": "kdenlive", "use": "computer"},
      {"choose": true, "use": "mcp"}
    ],
    "computerUse": "app"
  }
}
```

- `servers` names at most four applications, each once, by a short id: lowercase letters and digits in words joined by single hyphens, at most 40 characters (`blender`, `my-cad`, `kdenlive`), never `computer` or `chosen`. Any id is valid. Agentry Desktop resolves it on the person's computer to the application they approved with that id, or asks them which of theirs it is.
- Use a preset's id when the application is one of the library's (below), and otherwise the application's plain name in the grammar. The id is the kind of application, never a particular install.
- `use` says what of the application the run uses: `mcp` (its MCP server, the default), `computer` (its windows only, through computer use), or `both`. `computer` and `both` need `computerUse`. Left out, the entry is the server, and its windows too when the line declares `computerUse`.
- `{"choose": true, "use": ...}` is "an application the person chooses" when the run starts. Draft it when the description leaves the application open ("whichever editor I have open", "my 3D program"). At most one per line, with no id and no tools.
- An entry that uses the server may narrow its tools with `"tools": [...]`, never widen them. Narrow only to tools you know the application's server lists (a preset's, below): a run whose narrowing names a tool the server does not list is refused when it starts. Without `tools` the run has every tool the server lists.
- `computerUse` is `app` (the windows of the applications whose windows it uses, or of one the person chooses when it names none) or `screen` (the whole screen). Draft `app`; `screen` needs the person's own setting or flag, and a description rarely needs it.
- Leave out `computerUse` unless the task needs to see or click in the application's window; the application's own tools do most work. An application with no MCP server is reached only through its windows: `use: computer`.

## The library's presets

Agentry Desktop's library sets up popular applications in one click. Their ids are known, so a draft for one of them uses that id, and their tools are known, so the instructions can name them [src: web:https://docs.secureagentry.net/quickagents/]:

| id | Application | Tools and their class |
| --- | --- | --- |
| `blender` | Blender, through its MCP add-on | `get_scene_info`, `look`, `get_addon_status`, `search_assets` read; `generate_3d`, `import_asset` act (they reach asset services from the person's computer); `execute_blender_code` exec (Python inside Blender) |
| `unreal` | Unreal Engine 5.8, through Epic's Unreal MCP plugin | `list_toolsets`, `describe_toolset` read; `call_tool` act |

They are examples, not a list of what is allowed. For any other application the server's tools are known only on the person's computer: the run lists them under the server's own names, and a tool the person has not classed asks them before each call.

## What the person is asked

Installing the QuickAgent asks the person, in these words, for each application: "the application blender on your computer, through its MCP server (every tool it lists)", or "(tools get_scene_info, look)" for a narrowing, or "through its windows only"; and for computer use, the windows or the screen. Agentry Desktop adds what it knows: the preset's name and tools, or "set up by you; Agentry has not vetted it". The run asks again before each call that runs code, unless the person allows every one for that run, and the person can pause or stop the agent's use of their computer at any time. The receipt says what crossed and what the person's computer is outside of: the run's network and file rules do not reach it [src: web:https://docs.secureagentry.net/trust/].

## Writing the instructions

- Name a preset's tools as the table does (`get_scene_info`, `look`). For any other application, say what to do and let the agent find the tools the run lists under the application's entry.
- Read before acting: look at the application's state first, then make the changes the person asked for.
- Prefer tools that act directly to tools that run code (`execute_blender_code`). When code is needed, keep it short and say what it does: the person reads it before it runs.
- A call can be refused: the application is not open or its server is not running (the refusal names the step), the person declined the call, or they paused or stopped the agent. Say so in the result and go on with what can be done; never try the same thing another way.

## Where it runs

On the person's computer with Agentry Desktop running and the application added and approved there, and on the person's own runhost while their computer follows the run. An Agentry Desktop that predates its list of approved applications binds only `blender` and `unreal`, so a person with an older one updates it to use any other. Never In Agentry: a hosted run never reaches anyone's computer [src: web:https://docs.secureagentry.net/runner/].

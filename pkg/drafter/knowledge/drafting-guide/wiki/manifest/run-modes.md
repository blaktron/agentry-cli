---
title: Run modes
type: guide
summary: Autonomous and interactive runs, when a draft adds interactive, what a conversation can and cannot do, and the opening line.
sources: [web:https://docs.secureagentry.net/quickagents/, web:https://docs.secureagentry.net/agentry/hosted-runs/]
updated: 2026-10-12
---

# Run modes

`modes` lists how a QuickAgent can run [src: web:https://docs.secureagentry.net/quickagents/]:

- `["autonomous"]`, the default: a run starts from the task and the person's files, after the intake's questions, does the job, returns its result and ends.
- `["interactive"]`: the run is a conversation. In the desktop it is the run panel's chat, in Agentry a session (*Talk to it*), in the CLI a chat in the terminal. The agent can ask for what the work turns out to need, and the person can answer and add files to the run's read-only inputs before asking for the final result.
- Both: the person chooses when they start it.

## When to add interactive

Add `interactive` only when the description asks for a conversation, a chat or back-and-forth: practising an interview, a tutor, a helper the person talks a problem through with. What the person names or gives the QuickAgent ("a city I name", "the notes I paste", "the repository I choose") is the task or an input of an autonomous run, not a reason for a conversation. A description that asks to approve each change before it is made (on a server, say) asks for one.

## What a conversation cannot do

A conversation has exactly the contract an autonomous run of the same release has: the same network, commands, keys, servers and files. Nothing the person says in it gives the agent more access, so declare in the manifest everything the conversation will use [src: web:https://docs.secureagentry.net/quickagents/].

## The opening line

With `interactive`, add `interactive.opening`: the first line the agent says when a session starts with no message from the person, at most 500 characters. Make it ask for what the work starts from ("Which incident, and what do you need from it?").

The instructions of an interactive QuickAgent say how to conduct the conversation in `## Steps`: what to ask first, when to ask again, and when the work is done and the final result is written [src: web:https://docs.secureagentry.net/agentry/hosted-runs/].

---
title: Writing the instructions
type: guide
summary: How to write the AGENT.md a draft returns, the instruction its QuickAgent's agent follows; the six headings in order, what each holds, how much to write, and what to leave out because the System Prompt already says it.
sources: [web:https://docs.secureagentry.net/runner/, web:https://docs.secureagentry.net/quickagents/]
updated: 2026-10-05
---

# Writing the instructions

The instructions are the `AGENT.md` the QuickAgent's agent follows on every run. Write them in your own words. Draw on what you read, but never copy a page's text, and list every source you used in `sources`.

## What they must carry

Most QuickAgents run with no network and no web search, so the instructions carry the domain knowledge the run cannot look up: what a capable model would need from an expert to do this task well, the way a full skill for the task would teach it. Write it so a capable model with only the declared tools can do the job.

The runner already tells every run about its sandbox, its inputs under `/inputs`, the intake answers, its tools, how to write its reports and how to treat untrusted text (see [How a QuickAgent run works](../runtime/how-a-run-works.md)). Leave all of that out [src: web:https://docs.secureagentry.net/runner/].

## How much

As much as the task warrants: a few kilobytes for a small task, 20 to 30 KB for a big one, and **at most 32,768 bytes**. A file past that is refused and sent to a repair turn to be shortened, which costs more than writing it to size the first time.

## The six headings

Use these six headings, in this order, every time, even when a part is one line:

- `## Role`: one or two sentences.
- `## Inputs`: the task, each intake answer and input file by what it means, and what to do when one is missing.
- `## Steps`: numbered, each one action, with the decision rules for that step: what to check, what to choose, and when to stop. Name the tool a step uses (`read_input`, `fetch`, a server's tool by its exact name, `run_command` with the command).
- `## Output`: exactly what it returns, and in what format, with a skeleton to copy when the format has parts.
- `## Background`: the domain facts the steps rely on: definitions, formats, standards, thresholds, procedures and short worked examples, in your own words.
- `## Checks`: missing or malformed input, the edge cases, and the checks the result must pass before it is returned.

## What it does and does not do

- It returns its result; it does not post, send or upload anything unless the description asks for that.
- It uses only what the manifest declares. A step that needs a host, a command, a key or a server the manifest does not declare will fail in the run, and reads to the registry's scan as an undeclared capability (see [What checks a draft](review.md)).
- Facts you were not able to check, write as defaults the person can change, never as lookups the run should make.
- It never tells the agent to ignore, override or reveal its instructions, and never calls anything "safe" or "secure".

Worked examples of whole drafts are on [Worked drafts](examples.md).

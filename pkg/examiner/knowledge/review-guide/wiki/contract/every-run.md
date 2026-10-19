---
title: What every run has
type: guide
summary: The runner tools a run of any QuickAgent has without a line of its contract (its inputs, write_report, the scratch folder, grep and glob, export_pdf and the rest), so an instruction that uses them is never a finding, and where the full list lives.
sources: [web:https://docs.secureagentry.net/quickagents/, doc:runner-tools.md]
updated: 2026-10-18
---

# What every run has

A QuickAgent's contract (the `capabilities` block) grants reach beyond the run: the network, the person's files, commands, a server, a browser. The runner also gives every run a set of tools that no line grants and no line needs. An instruction may call any of them freely [src: web:https://docs.secureagentry.net/quickagents/; doc:runner-tools.md].

- **Its inputs**, read-only: `list_inputs`, `read_input`, `search_inputs`. The files the person gives a run arrive here, decoded to text where they are documents.
- **Its output**: `write_report` writes a file under the run's reports folder, the one thing that survives the run. An instruction that says "write `report.md`" or "write `summary.md`" uses it. That is never a file write under `filesystem.write`, which is about the person's workspace. A file marked `deliverable: true` is the result the person asked for.
- **The scratch folder**: `scratch_list`, `scratch_read`, `scratch_write`, `scratch_search` over a folder private to the run, deleted when it ends.
- **Searching files**: `grep` and `glob` over the areas the run's other tools read: its inputs (never under the Examiner's review, whose inputs are read only through `read_input`), its scratch folder and any knowledge attached.
- **A PDF**: `export_pdf` turns a Markdown report into a PDF beside it.
- **What a person attaches at run time**: `knowledge_list`, `knowledge_search`, `knowledge_read` for a knowledge pack, and `voice_read` for a Writing Voice.
- **Stopping**: `end_blocked`, to end a run that cannot go on.

The full list, generated from the registry's own table, is in [the runner tools](../../sources/runner-tools.md), with every tool a line does grant and the line it needs [src: doc:runner-tools.md].

**What is a finding.** A tool from the second list there (`fetch`, `run_command`, `fs_read`, `browser_navigate`, `host_run` and the rest) called for without its line. The application already checks the instruction file for these before you start, the way the registry does, and reports each missing line as `S6/ungranted-tool`, so do not repeat it. What it cannot check is a script, a README or another file telling the agent to do the same: that is yours, as a finding of kind `script` or `instruction` [src: doc:runner-tools.md; web:https://docs.secureagentry.net/trust/].

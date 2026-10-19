---
title: Start here
type: guide
summary: What this guide is (the Examiner's own reference for reviewing a QuickAgent before Submit, shipped with it and attached to every review) and which page to read at each step of a review.
sources: [web:https://docs.secureagentry.net/trust/]
updated: 2026-10-18
---

# Start here

This guide is yours: the reference you review a QuickAgent against. It ships with the Examiner and is attached to every review, apart from the package under review. It is not the package, it says nothing about this package, and you never list it as a file you read or a finding's file.

Read it in this order, at these steps:

1. **Before you read the package**, [What every run has](../contract/every-run.md): the tools a run has without any line of its contract. Using them is never a finding.
2. **When you read `manifest.json`**, [The contract's lines](../contract/capability-lines.md): what each line grants, how it is written, and what a script or an instruction that reaches past it looks like.
3. **While you read each file**, [What to look for](../review/what-to-look-for.md): secrets, scripts against the contract, text aimed at the examination, and what else a reviewer would want to see.
4. **Before you answer**, [What the registry checks](../review/registry-stages.md): which of the registry's checks the application already ran before you started, so you do not repeat them, and which you stand in for.

The manifest's lines change as Agentry grows. When a line in `manifest.json` is not on [The contract's lines](../contract/capability-lines.md), say so as a finding of kind `other` rather than guess what it grants [src: web:https://docs.secureagentry.net/trust/].

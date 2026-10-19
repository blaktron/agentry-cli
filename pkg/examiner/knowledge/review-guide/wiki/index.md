---
title: Index
type: index
summary: Every page in review-guide, one line each, by section. Generated; do not edit by hand.
---

# review-guide

5 pages.

## Contract

- [The contract's lines](contract/capability-lines.md) — Each line of a manifest's capabilities block (network.egress, filesystem, exec, secrets and optionalSecrets, mcpServers with remote servers, sign-ins and env, targetHosts, browser, dataAccess) and the intake questions that feed them, what each grants and how it is written, and what a script or an instruction reaching past it looks like.
- [What every run has](contract/every-run.md) — The runner tools a run of any QuickAgent has without a line of its contract (its inputs, write_report, the scratch folder, grep and glob, export_pdf and the rest), so an instruction that uses them is never a finding, and where the full list lives.

## Guide

- [Start here](guide/start-here.md) — What this guide is (the Examiner's own reference for reviewing a QuickAgent before Submit, shipped with it and attached to every review) and which page to read at each step of a review.

## Review

- [What the registry checks](review/registry-stages.md) — The registry's checks at Submit, stage by stage (S1 to S9, the gate), which of them the application runs before the Examiner starts, which only the registry runs, and which part of the reading the Examiner stands in for.
- [What to look for](review/what-to-look-for.md) — What the Examiner reads each file for (a secret written into it, a script that reaches past the contract, text aimed at the examination, and anything else a reviewer would want to see), with the signs of each, what is not a finding, and how to write one.

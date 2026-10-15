---
title: What checks a draft
type: guide
summary: The checks a draft meets after you answer (the rules, AI Review's check of the finished draft, one repair turn) and the scan that decides whether a submitted QuickAgent publishes, goes to review or is rejected. Today's facts; rewritten when the scan's rules on describing risk change.
sources: [web:https://docs.secureagentry.net/trust/, web:https://docs.secureagentry.net/agentry/builder/]
updated: 2026-10-15
---

# What checks a draft

This page states today's checks. It is rewritten when the scan's rules for instructions that describe risk, rather than ask for harm, change.

## After you answer

1. **The rules.** The registry's own rules check the manifest, and the Drafter's rules check the rest: the bounds, the six headings and size of the instructions, the commands (never an SSH client, which a target host replaces, nor a browser of its own, as a command, an MCP server package or a hosted browser service by its address, which a declared browser replaces), that the instructions make a PDF only with `export_pdf` (never with `write_report`, PDF syntax written by hand, or a page left for the person to print), and that every MCP server pin is published, runs on the runtime, and is neither on [Does not run here](../servers/does-not-run-here.md) nor depends directly on a package listed there. An npm install script is a note for the person, not a refusal. The notes say what admitted each server: its *Runs here* entry, or your judgement with this guide. Small slips (a stray empty field, a duplicate tag) are corrected and named in the notes.
2. **AI Review's check of the finished draft.** It asks whether the draft does the task it was given, and reads the draft against the same rubric the registry's scan uses. A draft read as not doing its task is flagged. Its other findings are notes for the person [src: web:https://docs.secureagentry.net/agentry/builder/].
3. **One repair turn.** A draft the rules refuse, or AI Review flags as not doing its task, gets one more turn with the report. Your instruction's *A repair turn* says what that turn may change.

The person then reads a short summary of the draft, with one line on AI Review's check, and decides whether to submit it [src: web:https://docs.secureagentry.net/agentry/builder/].

## When the person submits it

A submitted draft goes through the same scan as any release, on the exact bytes that ship. Then the gate decides: a version whose findings are low or medium publishes, a high finding sends it to a person for review, and a critical one is rejected. A draft that goes to review stays private until it is published [src: web:https://docs.secureagentry.net/trust/; web:https://docs.secureagentry.net/agentry/builder/].

The stages that most often hold a draft:

- **Secrets (S2).** A key, a token or a password anywhere in the files, an `env` value included.
- **Hidden content (S3).** Zero-width or bidirectional characters, HTML comments, encoded blobs. Write plain text.
- **Injection signatures (S4) and AI Review (S5).** Text that reads as telling the consuming agent to override its instructions, to send data somewhere, to use a capability it was not declared, to deceive the person or to destroy something. Today a QuickAgent whose task is about attacks (a review of prompts, a phishing triage) can be held for review even when its instruction only describes them. Describe such patterns in your own words; never quote a working attack string, a credential pattern or an override phrase.
- **Declared MCP servers (S7).** A Critical advisory on a pinned version rejects it, and a High one with a CVSS score of 8.5 or more, or none, sends it to review. Read the registry's check of the version before you pin it [src: web:https://docs.secureagentry.net/trust/].
- **An undeclared capability (S5, S9).** An instruction that uses a host, a command or a key the manifest does not declare can be read by AI Review as an undeclared capability, which is a high finding. When a later version of the QuickAgent is published, S9 compares it with the one before, and a host its content newly reaches without declaring it is a finding there too (see [Network and fetch](../manifest/egress-and-fetch.md)) [src: web:https://docs.secureagentry.net/trust/].

Never call the QuickAgent or anything it uses "safe" or "secure", in any file. Say what it does, what it was checked for and what it cannot do.

---
title: What the registry checks
type: guide
summary: The registry's checks at Submit, stage by stage (S1 to S9, the gate), which of them the application runs before the Examiner starts, which only the registry runs, and which part of the reading the Examiner stands in for.
sources: [web:https://docs.secureagentry.net/trust/]
updated: 2026-10-18
---

# What the registry checks

When a QuickAgent is submitted, the registry scans it in stages and a gate decides: published, held for a person, or rejected. This review is not that scan, and the registry's answer at Submit is the one that counts [src: web:https://docs.secureagentry.net/trust/].

**Already run here, before you start.** The application runs these with the registry's own code, and its report carries their findings beside yours:

- the manifest rules;
- the inventory (S1);
- the hidden-content and injection rules (S3 and S4);
- the check of the instruction file against the runner tools each contract line grants (part of S6);
- the registry's record of each declared server (S7), when the registry answers.

AI Review (S5) is the call you make with `ai_review`. Its six questions are the registry's, and the application reads its answer from the run's own record.

**Only the registry runs:**

- the secrets stage (S2), with its own tools over the files and their history;
- the scripts stage's reading of each script (the rest of S6);
- the publisher signals (S8): a name that imitates a popular QuickAgent, suspicious links, keyword stuffing, and the account's own record;
- the diff against the last published version (S9);
- an organization's own policy.

**What you stand in for.** The registry's secrets stage reads every file, and its scripts stage every script, mechanically. You read the same files as a reviewer would: for a secret, for a script that reaches past the contract, for text aimed at the examination, and for anything else worth a look ([What to look for](what-to-look-for.md)). What you find is reported as your reading and is never counted in the outcome, which comes from the registry's rules and AI Review alone [src: web:https://docs.secureagentry.net/trust/].

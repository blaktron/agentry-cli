---
name: verify-change
description: Prove a merged change does what its issue asked, by deploying it to the dev server or running it, checking the changed behaviour itself, and saying plainly what is still unverified. Use after a merge, before you close an issue, or when the person asks whether something works.
license: MIT
---

# Verify a change

CI proves the tests pass. Verification proves the change exists where the
person will use it and does what the issue asked.

1. **List what to prove.** Take the issue's acceptance, one line per thing it
   asked for. Each line gets a check, or the word "unverified" and the reason.
2. **Put the change where it runs.**
   - **A dev server:** deploy the merged commit, never the worktree's
     uncommitted state. Use the project's own deploy steps (from its rules,
     or yours kept in data/notes/<project>.md), with the host tools. Note
     what you changed on the server. Clear any cache in front of it the
     project's steps name.
   - **No dev server:** build the merged commit and run it in your sandbox: a
     command, a test against the built program, a server on a port the
     person added, so they can open it too.
   - **A library or a tool with nothing to run:** the tests are the
     verification; say so, and say which behaviour no test covers.
3. **Check the behaviour itself**, the way a person would meet it: the page
   in the browser (a public address, or the port the person added), the
   command's output, the API's answer. Read what it shows. Check one thing the
   change could have broken next to it, too.
4. **Confirm what is running is the change**: the commit or version the dev
   server reports, the build's timestamp, a log line. A check against the old
   build proves nothing.
5. **Report**, for each line of step 1: the check, what it showed, and
   verified or unverified. Then what you did not check and why. "Not
   verified" beats a claim that will turn out wrong.

If a check fails, the change is not done: say so, reopen the work (a fix on a
new branch, through the loop again), and never close the issue on it.

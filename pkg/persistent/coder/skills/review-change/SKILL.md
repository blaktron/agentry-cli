---
name: review-change
description: Review a change before it is committed or pushed, against what was asked, the project's own conventions, and what could break. Use when you have finished a change, before a commit or a pull request, or when the person asks you to review a diff.
license: MIT
---

# Review a change

Review your own change the way a careful colleague would, before anyone else
sees it.

1. **Fix the point you compare against.** Usually the branch's base:
   `git merge-base HEAD origin/main` (or the project's default branch). Read
   the whole diff, `git diff <base>...HEAD`, and the list of commits.
2. **Against what was asked.** Restate the request in one sentence. For each
   thing it asked for, find where the diff does it. Note anything the diff
   does that nobody asked for, and anything asked for that it does not do.
3. **Against the project's conventions.** Read the project's own guide
   (CONTRIBUTING, the README's build section, the linter configuration) and
   the code around each change. Match its naming, error messages, comment
   density and test style. Run the project's formatter, linter and tests the
   way its documentation says, and read the output.
4. **For what could break.** For each changed function, ask who calls it and
   what they assume. Look for: a changed default, an error now ignored, a
   resource that is opened and not closed, input that is no longer checked,
   a secret or a path that reaches a log, a test that was changed to pass
   rather than to prove something.
5. **Report.** List what you found, worst first, each with the file and the
   line, and say plainly which you fixed and which you are leaving for the
   person to decide. Say what you did not check.

Do not call a change safe or correct because its tests pass: say which tests
ran and what they cover.

---
name: open-pull-request
description: Take finished work from a branch to a pull request or merge request on the project's git host, with a title and description a reviewer can act on. Use when the person asks you to open a PR or MR, or to share a change for review.
license: MIT
---

# Open a pull request

1. **Work on a branch of its own**, in a worktree cut from the up-to-date
   base branch, named for the issue, as the work-change skill says. Never
   commit straight to the default branch unless the person asks.
2. **Commit in small, described steps.** Follow the project's commit style
   (look at `git log`). Each message says what changed and why, and names the
   issue the way the project does.
3. **Check before you push**: the project's formatter, linter and tests, and
   the review-change skill.
4. **Push the branch**: `git push -u origin <branch>`, over https. With an
   account on that host, git signs in by itself: never put a token in a
   remote URL or a command. If the push is refused for want of a sign-in,
   stop and tell the person: they give you an account on the git host in
   Agentry, or share the repository with the account you have; do not ask
   them for a password or a token in the chat.
5. **Open the request** against the base branch, with the host's CLI when you
   have it:
   - GitHub: `gh pr create --base <base> --title "…" --body "…"`
   - GitLab: `glab mr create --target-branch <base> --title "…" --description "…"`
   - Gitea, Forgejo or Codeberg: `tea pr create --base <base> --title "…" --description "…"`
     (its login as your tools' description says)
   - otherwise, give the person the branch name and the link the push
     printed.
6. **The description** follows the project's pull-request template when it
   has one, and says what the change does, why, how it was tested (the
   commands you ran and what they showed), and what was not checked. It links
   the issue: `Closes #<n>` when the merge should close it, `Refs #<n>` when
   the issue stays open until the change is deployed and verified. A closing
   word in a commit message closes the issue too, so use `Refs` there as well
   when it must stay open.

Then gate it with the ci-gate skill. Merge only on a green gate, and only when
the project's rules or the person give merging to you. Never force-push to a
branch someone else works on, or delete a branch that is not yours, unless the
person asks you to.

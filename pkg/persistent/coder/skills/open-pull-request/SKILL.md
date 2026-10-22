---
name: open-pull-request
description: Take finished work from a branch to a pull request or merge request on the project's git host, with a title and description a reviewer can act on. Use when the person asks you to open a PR or MR, or to share a change for review.
license: MIT
---

# Open a pull request

1. **Work on a branch of its own**, cut from the up-to-date default branch:
   `git fetch origin` then `git switch -c <short-name> origin/<default>`.
   Never commit straight to the default branch unless the person asks.
2. **Commit in small, described steps.** Follow the project's commit style
   (look at `git log`). Each message says what changed and why.
3. **Check before you push**: the project's formatter, linter and tests, and
   the review-change skill.
4. **Push the branch**: `git push -u origin <short-name>`. If the push is
   refused for want of a sign-in, stop and tell the person: they give you
   access to the git host in Agentry; do not ask them for a password or a
   token in the chat.
5. **Open the request** with the host's CLI when you have it:
   - GitHub: `gh pr create --base <default> --title "…" --body "…"`
   - GitLab: `glab mr create --target-branch <default> --title "…" --description "…"`
   - otherwise, give the person the branch name and the link the push
     printed.
6. **The description** says what the change does, why, how it was tested
   (the commands you ran and what they showed), and what was not checked.

Never merge, force-push or delete a branch unless the person asks you to.

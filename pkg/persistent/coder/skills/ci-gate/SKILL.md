---
name: ci-gate
description: Wait for a pull request's CI honestly before merging, judging the checks of the commit you pushed and nothing else. Use after every push to a pull request when a merge is next, or when CI looks stuck, red or never starts.
license: MIT
---

# Gate on CI honestly

The gate is what CI said about **the commit you pushed**. Not what an earlier
run said, not a check list read before the new run started, and not the exit
status of a pipeline.

1. **Name the commit.** In the worktree, `sha=$(git rev-parse HEAD)` after the
   push, and check that the pull request's head is that commit (`gh pr view
   <n> --json headRefOid`, `glab mr view <n>`). A gate started seconds after a
   push can read the previous head, whose run was just cancelled.
2. **Wait in the background.** A run can take longer than a foreground
   command may, so start the wait as a background program that writes to a
   file, and read the file when it ends:
   - GitHub: `gh pr checks <n> --watch --fail-fast > data/notes/gate-<n>.log 2>&1; echo "exit $?" >> data/notes/gate-<n>.log`
   - GitLab: `glab ci status --live --branch <branch>` the same way, or poll
     `glab ci get --branch <branch>` until the pipeline for `$sha` finishes.
   - Another host: poll its API or CLI for the commit's status every minute
     or two. Never poll faster.
3. **Read the result, not the pipe.** The gate is green only when every
   required check of `$sha` succeeded. Never pipe the watch into `tail`,
   `grep` or anything else in the command whose status decides the merge: a
   pipeline's exit status is its last command's, so a red gate reads as
   green. Capture the status itself, and merge only inside
   `if [ "$st" -eq 0 ]`.
4. **When it is red**, read the failing job's log (`gh run view <id>
   --log-failed`, `glab ci trace <job>`). Fix the cause on the same branch,
   push, and gate the new commit from step 1. A check that failed for a reason
   outside the change (a runner that went away, a flaky network) may be
   re-run once; say that you did and why. Never change a test to make it pass
   rather than to prove something, and never skip or switch off a check.
5. **When nothing starts**, look before waiting longer: a pull request that
   conflicts with its base often gets no run at all (`gh pr view <n> --json
   mergeable`); bring the base in, push, gate again. A run waiting on approval
   or a runner is the person's to unblock: say so.

Report the commit, each check and its result, and the command you read them
with. "Green" without that is a claim, not a gate.

#!/usr/bin/env python3
"""Fail when a workflow lets a fork's pull request reach a self-hosted runner.

A self-hosted runner persists between jobs, so code a stranger's pull request
brings must never run on one. Every job of a workflow triggered by
`pull_request` or `pull_request_target` is checked. A job that can run on a
self-hosted runner must be in one of these forms, and nothing looser:

  * it carries a fork guard in its `if`:
      - `github.event_name != 'pull_request' || <same-repository test>`, which
        is enough only when the workflow has no `pull_request_target` trigger
        (the event name of a `pull_request_target` run is not
        `pull_request`, so that form would let it through);
      - `github.event_name == 'pull_request' && <same-repository test>`;
      - the bare <same-repository test>;
    where the same-repository test is
    `github.event.pull_request.head.repo.full_name == github.repository`.
    Whitespace and an enclosing `${{ }}` are ignored; any other text in the
    condition is not accepted.
  * its `runs-on` is exactly the visibility-conditional form below, which is
    self-hosted while the repository is private (only collaborators can open a
    pull request there) and a GitHub-hosted runner once it is public.

A job on a GitHub-hosted runner (`ubuntu-*`, `windows-*`, `macos-*`) needs no
guard. A `runs-on` that is an expression other than the conditional form, a
runner group, a label that names no GitHub-hosted image, and a job that calls
a reusable workflow (which has no `runs-on` of its own) are all treated as
able to run on a self-hosted runner.

Usage: workflow_guard.py [workflow.yaml ...]
With no argument it reads .github/workflows/*.y*ml under the current
directory. Needs python3 with PyYAML.
"""

import glob
import re
import sys

try:
    import yaml
except ImportError:  # pragma: no cover - the message is the behaviour
    print("workflow_guard.py needs python3 with PyYAML (pip install pyyaml)")
    sys.exit(2)

SAME_REPO = "github.event.pull_request.head.repo.full_name == github.repository"
GUARD_NOT_PR_OR = "github.event_name != 'pull_request' || " + SAME_REPO
GUARD_PR_AND = "github.event_name == 'pull_request' && " + SAME_REPO

# Safe in a workflow that has no pull_request_target trigger.
GUARDS_PULL_REQUEST_ONLY = {GUARD_NOT_PR_OR, GUARD_PR_AND, SAME_REPO}
# Safe whatever the workflow's pull-request triggers are.
GUARDS_ANY = {GUARD_PR_AND, SAME_REPO}

# The one runs-on expression accepted without a guard.
CONDITIONAL_RUNS_ON = (
    "github.event.repository.private && "
    "fromJSON('[\"self-hosted\",\"linux\",\"x64\"]') || 'ubuntu-latest'"
)

PR_TRIGGERS = {"pull_request", "pull_request_target"}
HOSTED_LABEL = re.compile(r"^(ubuntu|windows|macos)-(latest|[0-9][0-9a-z.\-]*)$")


def normalize(expr):
    """The expression without an enclosing ${{ }} and with one-space gaps."""
    text = " ".join(str(expr).split())
    if text.startswith("${{") and text.endswith("}}"):
        text = text[3:-2].strip()
    return text


def triggers_of(doc):
    # PyYAML reads the key `on` as the boolean True.
    on = doc.get("on", doc.get(True))
    if on is None:
        return None
    if isinstance(on, str):
        return {on}
    if isinstance(on, list):
        return {str(t) for t in on}
    if isinstance(on, dict):
        return {str(t) for t in on}
    return None


def runs_on_verdict(spec):
    """Return "hosted", "conditional" or the reason it may be self-hosted."""
    if "runs-on" not in spec:
        return "calls a reusable workflow, which picks its own runner"
    runs_on = spec["runs-on"]
    if isinstance(runs_on, dict):
        if "group" in runs_on:
            return "runs-on names a runner group"
        runs_on = runs_on.get("labels", [])
    labels = [runs_on] if isinstance(runs_on, str) else list(runs_on or [])
    if not labels:
        return "runs-on names no runner"
    if len(labels) == 1 and "${{" in str(labels[0]):
        if normalize(labels[0]) == CONDITIONAL_RUNS_ON:
            return "conditional"
        return "runs-on is an expression other than the visibility conditional"
    for label in labels:
        label = str(label)
        if "${{" in label:
            return "runs-on mixes an expression into its labels"
        if not HOSTED_LABEL.match(label):
            return f"runs-on names `{label}`, which is not a GitHub-hosted image"
    return "hosted"


def check_file(path):
    """Return the list of problems in one workflow file."""
    try:
        with open(path, encoding="utf-8") as f:
            doc = yaml.safe_load(f)
    except (OSError, yaml.YAMLError) as err:
        return [f"{path}: does not parse: {err}"]
    if not isinstance(doc, dict) or not isinstance(doc.get("jobs"), dict):
        return [f"{path}: not a workflow (no `on` or no `jobs`)"]
    triggers = triggers_of(doc)
    if triggers is None:
        return [f"{path}: not a workflow (no `on` or no `jobs`)"]
    if not PR_TRIGGERS & triggers:
        return []
    accepted = GUARDS_ANY if "pull_request_target" in triggers else GUARDS_PULL_REQUEST_ONLY
    problems = []
    for job, spec in doc["jobs"].items():
        if not isinstance(spec, dict):
            problems.append(f"{path}: job {job} is not a mapping")
            continue
        verdict = runs_on_verdict(spec)
        if verdict in ("hosted", "conditional"):
            continue
        if "if" in spec and normalize(spec["if"]) in accepted:
            continue
        problems.append(
            f"{path}: job {job} can run on a self-hosted runner ({verdict}) and "
            f"a fork's pull request can reach it; give it `if: {GUARD_NOT_PR_OR}` "
            f"(`{GUARD_PR_AND}` when the workflow also triggers on "
            "pull_request_target), or run it on a GitHub-hosted runner, or set "
            f"`runs-on: ${{{{ {CONDITIONAL_RUNS_ON} }}}}`"
        )
    return problems


def main(argv):
    paths = argv[1:] or sorted(glob.glob(".github/workflows/*.y*ml"))
    if not paths:
        print("no workflow files found: run it from the repository root")
        return 1
    problems = []
    for path in paths:
        problems.extend(check_file(path))
    for line in problems:
        print(line)
    if not problems:
        print(f"ok: {len(paths)} workflow files, no fork-reachable job on a self-hosted runner")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

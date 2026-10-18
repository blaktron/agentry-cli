"""Tests for workflow_guard.py: run with `python3 -m unittest discover -s scripts/ci`."""

import contextlib
import io
import os
import tempfile
import textwrap
import unittest

import workflow_guard as wg

SAME_REPO = "github.event.pull_request.head.repo.full_name == github.repository"
GUARD = f"github.event_name != 'pull_request' || {SAME_REPO}"
CONDITIONAL = (
    "${{ github.event.repository.private && "
    "fromJSON('[\"self-hosted\",\"linux\",\"x64\"]') || 'ubuntu-latest' }}"
)
SELF_HOSTED = "[self-hosted, linux, x64]"


def workflow(trigger="pull_request", runs_on=SELF_HOSTED, cond=None, extra_jobs=""):
    """A one-job workflow; `cond` becomes the job's `if`, quoted."""
    lines = [f"on:\n  {trigger}:\n    branches: [dev]\n" if trigger else "on: [push]\n"]
    lines.append("jobs:\n  build:\n")
    if cond is not None:
        lines.append(f"    if: {cond}\n")
    if runs_on is not None:
        lines.append(f"    runs-on: {runs_on}\n")
    lines.append("    steps:\n      - run: true\n")
    lines.append(extra_jobs)
    return "".join(lines)


class GuardTest(unittest.TestCase):
    def check(self, text):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "w.yaml")
            with open(path, "w", encoding="utf-8") as f:
                f.write(text)
            return wg.check_file(path)

    def passes(self, text):
        self.assertEqual(self.check(text), [])

    def fails(self, text, mentions="job build"):
        problems = self.check(text)
        self.assertEqual(len(problems), 1, problems)
        self.assertIn(mentions, problems[0])

    # An unguarded job is the case the check exists for.
    def test_unguarded_self_hosted_job_fails(self):
        self.fails(workflow())

    def test_unguarded_self_hosted_job_fails_on_pull_request_target(self):
        self.fails(workflow(trigger="pull_request_target"))

    def test_guarded_self_hosted_job_passes(self):
        self.passes(workflow(cond=GUARD))

    def test_guard_inside_an_expression_wrapper_passes(self):
        self.passes(workflow(cond="${{ " + GUARD + " }}"))

    def test_guard_folded_over_lines_passes(self):
        text = workflow(cond=">-\n      github.event_name != 'pull_request' ||\n      " + SAME_REPO)
        self.passes(text)

    def test_pull_request_only_guard_passes(self):
        self.passes(workflow(cond=f"github.event_name == 'pull_request' && {SAME_REPO}"))

    # The conditional runs-on form, exactly.
    def test_visibility_conditional_runs_on_passes(self):
        self.passes(workflow(trigger="pull_request_target", runs_on=CONDITIONAL))

    def test_visibility_conditional_runs_on_passes_on_pull_request(self):
        self.passes(workflow(runs_on=CONDITIONAL))

    def test_looser_conditionals_fail(self):
        variants = {
            "swapped branches": "${{ github.event.repository.private && 'ubuntu-latest' || fromJSON('[\"self-hosted\",\"linux\",\"x64\"]') }}",
            "negated test": "${{ !github.event.repository.private && fromJSON('[\"self-hosted\",\"linux\",\"x64\"]') || 'ubuntu-latest' }}",
            "no hosted fallback": "${{ github.event.repository.private && fromJSON('[\"self-hosted\",\"linux\",\"x64\"]') }}",
            "other labels": "${{ github.event.repository.private && fromJSON('[\"self-hosted\",\"macOS\",\"ARM64\"]') || 'ubuntu-latest' }}",
            "other test": "${{ github.event.repository.fork && fromJSON('[\"self-hosted\",\"linux\",\"x64\"]') || 'ubuntu-latest' }}",
            "extra clause": "${{ github.event.repository.private && fromJSON('[\"self-hosted\",\"linux\",\"x64\"]') || github.actor == 'x' || 'ubuntu-latest' }}",
            "always self-hosted": "${{ fromJSON('[\"self-hosted\",\"linux\",\"x64\"]') }}",
            "an input": "${{ inputs.runner }}",
            "a matrix value": "${{ matrix.os }}",
        }
        for name, expr in variants.items():
            with self.subTest(name):
                self.fails(workflow(trigger="pull_request_target", runs_on=expr))

    # Anything looser than the exact guard fails.
    def test_looser_guards_fail(self):
        variants = {
            "or true": f"{GUARD} || true",
            "or another actor": f"{GUARD} || github.actor == 'maintainer'",
            "and another clause": f"{GUARD} && github.ref == 'refs/heads/dev'",
            "guard in a string": f"contains('{SAME_REPO}', 'x')",
            "same test, not equal": SAME_REPO.replace("==", "!="),
            "wrong event": f"github.event_name != 'push' || {SAME_REPO}",
            "other repo field": "github.event.pull_request.base.repo.full_name == github.repository",
            "always": "always()",
            "true": "true",
        }
        for name, cond in variants.items():
            with self.subTest(name):
                self.fails(workflow(cond=cond))

    def test_event_name_guard_does_not_cover_pull_request_target(self):
        # A pull_request_target run never has the event name `pull_request`, so
        # the first clause of the guard is true for a fork's run.
        self.fails(workflow(trigger="pull_request_target", cond=GUARD))

    def test_both_triggers_need_the_stricter_guard(self):
        text = (
            "on:\n  pull_request:\n  pull_request_target:\n"
            f"jobs:\n  build:\n    if: {GUARD}\n    runs-on: {SELF_HOSTED}\n    steps:\n      - run: true\n"
        )
        self.fails(text)

    def test_same_repository_test_alone_covers_both_triggers(self):
        text = (
            "on:\n  pull_request:\n  pull_request_target:\n"
            f"jobs:\n  build:\n    if: {SAME_REPO}\n    runs-on: {SELF_HOSTED}\n    steps:\n      - run: true\n"
        )
        self.passes(text)

    # What does not need a guard.
    def test_github_hosted_runners_need_no_guard(self):
        for label in ("ubuntu-latest", "ubuntu-24.04", "ubuntu-24.04-arm", "windows-2025", "macos-14", "macos-latest"):
            with self.subTest(label):
                self.passes(workflow(runs_on=label))
                self.passes(workflow(trigger="pull_request_target", runs_on=f"[{label}]"))

    def test_workflow_without_a_pull_request_trigger_is_not_checked(self):
        self.passes(workflow(trigger=None))
        self.passes(workflow(trigger="push"))
        self.passes(workflow(trigger="schedule"))

    def test_trigger_given_as_a_string_or_a_list_is_checked(self):
        for on in ("pull_request", "[push, pull_request]"):
            with self.subTest(on):
                text = f"on: {on}\njobs:\n  build:\n    runs-on: {SELF_HOSTED}\n    steps:\n      - run: true\n"
                self.fails(text)

    # Runners that are not plainly GitHub-hosted count as self-hosted.
    def test_unknown_runner_shapes_fail(self):
        for name, runs_on in {
            "a custom label alone": "mac-1",
            "a linux label alone": "linux",
            "a runner group": "\n      group: builders",
            "a mapping with labels": "\n      labels: [self-hosted]",
            "a mixed list": "[ubuntu-latest, self-hosted]",
        }.items():
            with self.subTest(name):
                self.fails(workflow(runs_on=runs_on))

    def test_mapping_with_hosted_labels_passes(self):
        self.passes(workflow(runs_on="\n      labels: [ubuntu-latest]"))

    def test_reusable_workflow_call_needs_a_guard(self):
        text = "on: pull_request\njobs:\n  build:\n    uses: ./.github/workflows/other.yaml\n"
        self.fails(text)
        self.passes(text.replace("    uses:", f"    if: {GUARD}\n    uses:"))

    def test_missing_runs_on_fails(self):
        self.fails(workflow(runs_on=None))

    def test_each_job_is_judged_on_its_own(self):
        extra = f"  second:\n    runs-on: {SELF_HOSTED}\n    steps:\n      - run: true\n"
        problems = self.check(workflow(cond=GUARD, extra_jobs=extra))
        self.assertEqual(len(problems), 1, problems)
        self.assertIn("job second", problems[0])

    # Files that are not workflows fail instead of passing silently.
    def test_unparseable_file_fails(self):
        self.fails("jobs: [\n", mentions="does not parse")

    def test_file_without_jobs_fails(self):
        self.fails("on: pull_request\n", mentions="not a workflow")

    def test_file_without_triggers_fails(self):
        self.fails("jobs:\n  build:\n    runs-on: ubuntu-latest\n", mentions="not a workflow")

    # main() reads its arguments, or the repository's workflows by default.
    def run_main(self, args, cwd=None):
        out = io.StringIO()
        old = os.getcwd()
        try:
            if cwd:
                os.chdir(cwd)
            with contextlib.redirect_stdout(out):
                code = wg.main(["workflow_guard.py"] + args)
        finally:
            os.chdir(old)
        return code, out.getvalue()

    def test_main_reads_the_default_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            os.makedirs(os.path.join(tmp, ".github", "workflows"))
            path = os.path.join(tmp, ".github", "workflows", "ci.yml")
            with open(path, "w", encoding="utf-8") as f:
                f.write(workflow())
            code, out = self.run_main([], cwd=tmp)
            self.assertEqual(code, 1)
            self.assertIn("job build", out)
            with open(path, "w", encoding="utf-8") as f:
                f.write(workflow(cond=GUARD))
            code, out = self.run_main([], cwd=tmp)
            self.assertEqual(code, 0, out)

    def test_main_fails_when_there_are_no_workflows(self):
        with tempfile.TemporaryDirectory() as tmp:
            code, _ = self.run_main([], cwd=tmp)
            self.assertEqual(code, 1)

    # The repository's own workflows keep the rule.
    def test_this_repositorys_workflows_pass(self):
        root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
        code, out = self.run_main([], cwd=root)
        self.assertEqual(code, 0, out)


if __name__ == "__main__":
    unittest.main()

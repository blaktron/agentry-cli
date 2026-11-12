"""Tests for diff.py: run with `python3 -m unittest discover -s scripts/modelfacts`."""

import contextlib
import io
import json
import os
import tempfile
import unittest

import diff


def catalogue(fetched, providers):
    return {"source": "https://models.dev/api.json", "fetched": fetched, "providers": providers}


class DiffTest(unittest.TestCase):
    def run_diff(self, old, new, *extra):
        with tempfile.TemporaryDirectory() as d:
            paths = []
            for name, data in (("old.json", old), ("new.json", new)):
                p = os.path.join(d, name)
                with open(p, "w") as f:
                    json.dump(data, f)
                paths.append(p)
            body = os.path.join(d, "body.md")
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                code = diff.main(paths + ["--body", body, *extra])
            with open(body) as f:
                return code, f.read()

    def test_added_removed_and_repriced_models_are_listed(self):
        old = catalogue("2026-11-01", {"anthropic": {
            "a": {"ctx": 200000, "out": 8000, "in": 3, "o": 15},
            "gone": {"ctx": 100000, "out": 4000},
        }})
        new = catalogue("2026-11-10", {"anthropic": {
            "a": {"ctx": 200000, "out": 8000, "in": 2.5, "o": 15},
            "b": {"ctx": 1000000, "out": 64000},
        }})
        code, body = self.run_diff(old, new, "--today", "2026-11-10")
        self.assertEqual(code, 0)
        self.assertIn("**Added (1)**", body)
        self.assertIn("- `anthropic/b`", body)
        self.assertIn("**Removed (1)**", body)
        self.assertIn("- `anthropic/gone`", body)
        self.assertIn("- `anthropic/a`: input price 3 → 2.5", body)
        self.assertNotIn("output price", body)

    def test_a_price_that_appears_is_a_change(self):
        old = catalogue("2026-11-01", {"openai": {"m": {"ctx": 128000, "out": 16000}}})
        new = catalogue("2026-11-02", {"openai": {"m": {"ctx": 128000, "out": 16000, "in": 1, "o": 4}}})
        code, body = self.run_diff(old, new, "--today", "2026-11-02")
        self.assertEqual(code, 0)
        self.assertIn("input price none → 1", body)

    def test_nothing_changed_and_recent_is_no_refresh(self):
        same = {"google": {"g": {"ctx": 1048576, "out": 65536}}}
        code, body = self.run_diff(catalogue("2026-11-01", same), catalogue("2026-11-10", same), "--today", "2026-11-10")
        self.assertEqual(code, 3)
        self.assertIn("No model changed", body)

    def test_nothing_changed_but_old_is_still_refreshed(self):
        same = {"google": {"g": {"ctx": 1048576, "out": 65536}}}
        code, _ = self.run_diff(catalogue("2026-10-20", same), catalogue("2026-11-10", same), "--today", "2026-11-10")
        self.assertEqual(code, 0, "21 days after the old copy was fetched it is re-dated")
        code, _ = self.run_diff(catalogue("2026-10-21", same), catalogue("2026-11-10", same), "--today", "2026-11-10")
        self.assertEqual(code, 3, "20 days is not yet")

    def test_a_provider_that_appears_lists_its_models(self):
        old = catalogue("2026-11-01", {})
        new = catalogue("2026-11-02", {"vultr": {"v": {"ctx": 32000, "out": 4000}}})
        code, body = self.run_diff(old, new, "--today", "2026-11-02")
        self.assertEqual(code, 0)
        self.assertIn("- `vultr/v`", body)

    def test_an_unreadable_file_is_a_usage_error(self):
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(diff.main(["/nonexistent/old.json", "/nonexistent/new.json"]), 2)


if __name__ == "__main__":
    unittest.main()

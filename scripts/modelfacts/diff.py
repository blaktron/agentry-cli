#!/usr/bin/env python3
"""Say what changed between two snapshots of the model catalogue.

    scripts/modelfacts/diff.py OLD NEW [--body FILE] [--max-age DAYS] [--today YYYY-MM-DD]

OLD and NEW are two copies of pkg/runner/modelfacts/catalog.json. It prints,
and writes to FILE with --body, a summary in Markdown: the models added and
removed, and those whose window, output limit or prices changed, by provider.

It exits 0 when the snapshot should be refreshed: a model changed, or OLD
was fetched at least --max-age days before --today (default 21, so a list
that never changes is still re-dated well inside a release's 30-day limit).
It exits 3 when nothing changed and OLD is recent enough, 2 on a usage or
read error.
"""
import argparse
import datetime
import json
import pathlib
import sys

FIELDS = [("ctx", "window"), ("out", "output limit"), ("in", "input price"), ("o", "output price"),
          ("cr", "cache read price"), ("cw", "cache write price")]


def load(path):
    data = json.loads(pathlib.Path(path).read_text())
    return data.get("fetched", ""), data.get("providers", {})


def changes(old, new):
    """The models added, removed and changed between two providers maps, as
    lists of (provider, model) and (provider, model, [(field, before, after)])."""
    added, removed, changed = [], [], []
    for provider in sorted(set(old) | set(new)):
        a, b = old.get(provider, {}), new.get(provider, {})
        for model in sorted(set(a) | set(b)):
            if model not in a:
                added.append((provider, model))
            elif model not in b:
                removed.append((provider, model))
            else:
                diffs = [(label, a[model].get(key), b[model].get(key))
                         for key, label in FIELDS if a[model].get(key) != b[model].get(key)]
                if diffs:
                    changed.append((provider, model, diffs))
    return added, removed, changed


def show(v):
    return "none" if v is None else f"{v:g}" if isinstance(v, float) else str(v)


def summary(old_fetched, new_fetched, added, removed, changed):
    lines = [f"The model catalogue, `pkg/runner/modelfacts/catalog.json`, refreshed from models.dev: "
             f"fetched {new_fetched}, replacing the copy of {old_fetched or 'an unknown day'}.", ""]
    if not (added or removed or changed):
        lines.append("No model changed. The snapshot's date moves forward, so a release does not refuse it as stale.")
        return "\n".join(lines) + "\n"
    if added:
        lines += [f"**Added ({len(added)})**", ""] + [f"- `{p}/{m}`" for p, m in added] + [""]
    if removed:
        lines += [f"**Removed ({len(removed)})**", ""] + [f"- `{p}/{m}`" for p, m in removed] + [""]
    if changed:
        lines += [f"**Changed ({len(changed)})**", ""]
        for p, m, diffs in changed:
            what = "; ".join(f"{label} {show(before)} → {show(after)}" for label, before, after in diffs)
            lines.append(f"- `{p}/{m}`: {what}")
        lines.append("")
    return "\n".join(lines)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("old")
    ap.add_argument("new")
    ap.add_argument("--body", help="also write the summary to this file")
    ap.add_argument("--max-age", type=int, default=21, help="days after which an unchanged snapshot is still refreshed")
    ap.add_argument("--today", help="the day to measure the age against (default: today, UTC)")
    args = ap.parse_args(argv)
    try:
        old_fetched, old = load(args.old)
        new_fetched, new = load(args.new)
        today = datetime.date.fromisoformat(args.today) if args.today else datetime.datetime.now(datetime.timezone.utc).date()
    except (OSError, ValueError) as e:
        print(f"diff.py: {e}", file=sys.stderr)
        return 2
    added, removed, changed = changes(old, new)
    text = summary(old_fetched, new_fetched, added, removed, changed)
    print(text, end="")
    if args.body:
        pathlib.Path(args.body).write_text(text)
    try:
        age = (today - datetime.date.fromisoformat(old_fetched)).days
    except ValueError:
        age = args.max_age  # a copy with no readable date is refreshed
    return 0 if (added or removed or changed or age >= args.max_age) else 3


if __name__ == "__main__":
    sys.exit(main())

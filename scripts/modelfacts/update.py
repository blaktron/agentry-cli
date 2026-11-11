#!/usr/bin/env python3
"""Regenerate pkg/runner/modelfacts/catalog.json from models.dev.

The catalogue is what the runner knows about a hosted model without asking
anyone at run time: its context window, its output limit and its prices. It
is a trimmed copy of models.dev's public list (MIT, see LICENSE.models-dev
beside the catalogue), kept for the providers a policy can name.

    scripts/modelfacts/update.py              fetch https://models.dev/api.json
    scripts/modelfacts/update.py --from FILE  read a saved copy instead

The output has one model a line, sorted, so a refresh reads as a diff of the
models that changed.
"""
import argparse
import datetime
import json
import pathlib
import sys
import urllib.request

SOURCE = "https://models.dev/api.json"
# The policy's provider names, which are models.dev's own ids.
PROVIDERS = ["anthropic", "google", "huggingface", "openai", "openrouter", "poe", "vultr"]
OUT = pathlib.Path(__file__).resolve().parents[2] / "pkg/runner/modelfacts/catalog.json"


def number(v):
    return v if isinstance(v, (int, float)) and not isinstance(v, bool) and v >= 0 else 0


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--from", dest="src", help="a saved copy of api.json")
    ap.add_argument("--date", help="the day the copy was fetched (default: today)")
    args = ap.parse_args()

    if args.src:
        data = json.loads(pathlib.Path(args.src).read_text())
    else:
        req = urllib.request.Request(SOURCE, headers={"User-Agent": "modelfacts-update"})
        with urllib.request.urlopen(req, timeout=60) as res:
            data = json.load(res)

    providers = {}
    for name in PROVIDERS:
        entry = data.get(name)
        if not entry:
            sys.exit(f"models.dev has no provider {name!r}: its ids changed, and so must PROVIDERS")
        models = {}
        for mid, m in entry.get("models", {}).items():
            limit, cost = m.get("limit") or {}, m.get("cost") or {}
            ctx = number(limit.get("context"))
            if not ctx:
                continue
            facts = {"ctx": ctx, "out": number(limit.get("output"))}
            # Prices are dollars a million tokens. A model with no price
            # listed has none here either, never a zero.
            if number(cost.get("input")) or number(cost.get("output")):
                facts["in"] = number(cost.get("input"))
                facts["o"] = number(cost.get("output"))
                if "cache_read" in cost:
                    facts["cr"] = number(cost.get("cache_read"))
                if "cache_write" in cost:
                    facts["cw"] = number(cost.get("cache_write"))
            models[mid] = facts
        providers[name] = models

    fetched = args.date or datetime.date.today().isoformat()
    lines = ["{", f' "source": {json.dumps(SOURCE)},', f' "fetched": {json.dumps(fetched)},', ' "providers": {']
    for i, name in enumerate(sorted(providers)):
        lines.append(f"  {json.dumps(name)}: {{")
        models = providers[name]
        ids = sorted(models)
        for j, mid in enumerate(ids):
            comma = "," if j < len(ids) - 1 else ""
            lines.append(f"   {json.dumps(mid)}: {json.dumps(models[mid], separators=(',', ':'), sort_keys=True)}{comma}")
        lines.append("  }" + ("," if i < len(providers) - 1 else ""))
    lines += [" }", "}"]
    OUT.write_text("\n".join(lines) + "\n")
    print(f"{OUT.relative_to(OUT.parents[3])}: {sum(len(m) for m in providers.values())} models of {len(providers)} providers, fetched {fetched}")


if __name__ == "__main__":
    main()

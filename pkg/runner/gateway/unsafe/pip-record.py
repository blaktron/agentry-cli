"""Record what a pip install put in the Unsafe Mode venv (agentry-cli#490).

Reads the JSON report `pip install --report` wrote and prints one line per
installed distribution, "pip <name>==<version> sha256:<hash of the archive
pip downloaded>", for the receipt (plan unsafe-mode.md invariant 8). A
distribution whose report carries no hash is printed without one.
"""

import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    report = json.load(fh)
for item in report.get("install", []):
    meta = item.get("metadata", {})
    line = "pip %s==%s" % (meta.get("name", "?"), meta.get("version", "?"))
    hashes = item.get("download_info", {}).get("archive_info", {}).get("hashes", {})
    if "sha256" in hashes:
        line += " sha256:" + hashes["sha256"]
    print(line)

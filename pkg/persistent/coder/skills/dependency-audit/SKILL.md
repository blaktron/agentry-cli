---
name: dependency-audit
description: Check a project's dependencies for known vulnerabilities and its history for committed secrets, then report or fix what is found. Use before a release, after adding a dependency, or when the person asks whether a project is up to date or has leaked a key.
license: MIT
---

# Audit dependencies and secrets

## Known vulnerabilities

1. Find what the project depends on: its lockfiles and manifests
   (`go.mod`/`go.sum`, `package-lock.json`, `pnpm-lock.yaml`, `yarn.lock`,
   `requirements*.txt`, `poetry.lock`, `Cargo.lock`, `Gemfile.lock` and so on).
2. Scan them with the tool you have, in this order:
   - `dependency_scan`, if it is among your tools: OSV-Scanner offline,
     against OSV's databases on the person's computer. An error from it
     means the scan is incomplete, not that the project is clean;
   - `osv-scanner scan source -r .` if `osv-scanner` is on your PATH;
   - the ecosystem's own: `npm audit`, `pnpm audit`, `pip-audit`,
     `cargo audit`, `govulncheck ./...`, when installed.
   If none is available, say so; do not guess from memory which versions are
   vulnerable.
3. For each finding, give the package, the version in use, the advisory id,
   and the first version that fixes it. Upgrade only when the person asked
   you to fix, one package at a time, running the tests after each.

## Committed secrets

1. Scan the working tree and the history: `gitleaks git .` (or `gitleaks dir .`
   for a folder that is not a repository), if `gitleaks` is on your PATH.
2. Never print a secret you find. Report the file, the line or commit and the
   kind of secret, and tell the person to rotate it: removing it from the
   code does not take it out of the history.

Report what you scanned, with which tool and version, and what you could not
scan.

# agentry — the SecureAgentry CLI

The command-line client for [SecureAgentry](https://secureagentry.net):
publish, install, and verify single-purpose AI agents. Every install checks
the archive digest and DSSE signature before anything lands on your machine,
and `agentry validate` runs the same manifest rules and content scanners the
registry runs at publish time — offline.

This repository holds the CLI only. It depends on the core module
[`github.com/blaktron/agentry`](https://github.com/blaktron/agentry) for the
package format (`pkg/manifest`), the scanners (`pkg/rules`, `pkg/ruleset`),
deterministic archives (`pkg/archive`), attestation verification
(`pkg/attest`), and the generated API client (`pkg/apiclient`).

## Building

The core module is private; builders have access to all blaktron repos:

```bash
export GOPRIVATE='github.com/blaktron/*'
# authenticate git for github.com/blaktron (gh auth, a credential helper,
# or an insteadOf rewrite with a PAT), then:
make build          # -> bin/agentry
make check          # gofmt + vet + lint + tests + build
```

## Commands

`init` · `validate` · `login` · `publish` · `install` · `verify` · `search`
· `info` · `yank` — run `agentry --help` for the full surface. Server-side
end-to-end coverage lives in the main repo's `scripts/acceptance/m3-cli.sh`,
which builds this CLI against a live registry.

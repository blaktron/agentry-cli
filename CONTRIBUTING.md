# Contributing to the Agentry CLI

This repository holds `agentry`, the command-line client, and the local
runner that executes a QuickAgent under the capabilities its manifest
declares. The runner and the install path carry most of the security weight,
so changes there get a closer review. Most other work is ordinary Go.

## Before you start

- **Security issues do not go in pull requests or public issues.** See
  [SECURITY.md](SECURITY.md).
- Everyone who takes part follows the [Code of Conduct](CODE_OF_CONDUCT.md).
- For a large change, open an issue first. A change to the package format,
  the scanners or the API belongs in the core repository,
  [blaktron/agentry](https://github.com/blaktron/agentry), which this module
  depends on.

## Contributing from outside

Fork, branch from `dev`, and open a pull request against `dev`. `dev` is
where changes land, and `main` takes only promotions from `dev` (the
`branch-model` check fails any other pull request into `main`). A
maintainer approves the CI run for a first-time contributor. Sign off every
commit (see "Certificate of origin" below), keep a change to one concern,
and say in the pull request what you verified and how.

## Development setup

You need Go at the version named by the `go` line of `go.mod`. `agentry run`
also needs a container engine: Docker Engine, Docker Desktop or podman.
`golangci-lint` is optional, and `make lint` runs it when it is installed.

| Command | What |
|---|---|
| `make check` | lint, tests and build: run it before you push |
| `make build` | `bin/agentry`, with the version taken from `git describe` |
| `make test` | `go test -race ./...` |
| `make lint` | `gofmt -l`, `go vet ./...`, `golangci-lint run ./...` |
| `make dist` | the five release binaries into `dist/`, unsigned |
| `make accept-runner` | the runner acceptance scripts in `scripts/acceptance/` |

`make accept-runner`'s first two scripts need no Docker. The third needs a
Docker daemon and `ANTHROPIC_API_KEY`, and skips itself without them.

A live `agentry run` writes its reports and receipts to `./agentry-out`. Run
it from a scratch directory, not from inside your checkout, so run output
never ends up in a commit.

## The rules that matter

**The runner grants what the manifest declares, and nothing more.** A tool,
mount, network destination or secret reaches a run only through the run
plan that `pkg/runner` compiles from the release's declared capabilities and
the operator policy. A change that widens what a run can reach without a
declaration is a bug, whatever it fixes. A new capability needs a test that
shows an undeclared use is refused.

**Installs verify before anything lands.** The archive digest and the DSSE
signature are checked before a file is written (`pkg/trust`,
`pkg/installed`). A change to that path keeps its refusal tests passing, and
a new refusal gets its own test.

**Built-in QuickAgents are frozen bytes.** The packages the binary embeds
(the Drafter, the Examiner, the Knowledge Creator and the Voice Writer) are
pinned by digest in `pkg/runner/localrun/builtins.lock`. A change to their
files is a new version of that package: update the lock in the same pull
request, and `TestTheBuiltinsLockHoldsEveryBuiltin` holds it.

**Do not log secrets.** No tokens, keys or granted secret values in logs,
receipts, error text or test output.

## Code style

Match the file you are editing. Beyond that:

- wrap errors with context (`fmt.Errorf("install %s: %w", ref, err)`);
- `context.Context` is the first parameter, threaded through;
- no package-level mutable state;
- comments explain constraints and why, not what the next line does;
- conventional commits (`feat:`, `fix:`, `perf:`, `security:`, `deps:`,
  `docs:`, `refactor:`, `test:`, `ci:`, `chore:`). A release's notes are
  generated from the merged pull requests, so the prefix is what a reader
  sees.

## Tests

New code needs tests at the level where the behaviour lives: table tests for
pure logic, and engine or acceptance tests for anything that drives a
container. Fixtures for hostile content are text only, defanged, and use
`*.invalid.example` hosts. Never use functional malware, real endpoints or
real secrets. Test values that look like secrets are allowlisted narrowly in
`.gitleaks.toml`, by file and value.

## Certificate of origin

This project uses the [Developer Certificate of Origin](https://developercertificate.org/)
instead of a CLA. Sign off your commits:

```bash
git commit -s -m "fix(runner): refuse a symlinked mount source"
```

By signing off you state that you wrote the contribution or have the right
to submit it under the project's licence.

## Licensing

The project licence is MIT ([LICENSE](LICENSE)). Do not add dependencies
under the AGPL or the GPL. A new dependency needs a line in the pull request
that explains why an existing one will not do.

## Releasing

Releases are cut by the maintainers. Each one is a `vX.Y.Z` tag (or a
`vX.Y.Z-dev.N` prerelease) with a GitHub Release that carries the signed
binaries, `SHA256SUMS` and its minisign signature. How to check them:
<https://docs.secureagentry.net/agentry/downloads/>.

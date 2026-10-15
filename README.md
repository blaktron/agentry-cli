# agentry: the Agentry CLI

`agentry` is the command-line client for [Agentry](https://secureagentry.net).
It installs, runs, publishes and drafts QuickAgents, and it contains the local
runner that executes a QuickAgent under the capabilities its manifest
declares. Every install checks the archive digest and the DSSE signature
before anything lands on the machine.

This repository holds the CLI and the runner. It depends on the core module
[`github.com/blaktron/agentry`](https://github.com/blaktron/agentry) for the
package format (`pkg/manifest`), the scanners (`pkg/rules`, `pkg/ruleset`),
deterministic archives (`pkg/archive`), attestation verification
(`pkg/attest`) and the generated API client (`pkg/apiclient`).

## Prerequisites

- Go, at the version named by the `go` line of `go.mod`.
- Docker, for `agentry run` with the default container adapter.
- `golangci-lint`, optional: `make lint` runs it when it is installed.

## Build, test, lint

```bash
make build    # bin/agentry, with the version taken from git describe
make test     # go test -race ./...
make lint     # gofmt -l, go vet ./..., golangci-lint run ./...
make check    # lint, test and build
make dist     # the five release binaries into dist/
make clean    # remove bin/
```

`make dist` builds `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64` and `windows/amd64` with `CGO_ENABLED=0`. It reads no signing
input. `DIST_VERSION` sets the version string and `AGENTRY_DEFAULT_REGISTRY`
bakes in the registry the binary falls back to when none is configured.

`make accept-runner` runs the runner acceptance scripts under
`scripts/acceptance/`. The first two need no Docker; the third needs a Docker
daemon and `ANTHROPIC_API_KEY`, and skips itself without them.

## Layout

| Path | Contents |
|---|---|
| `cmd/agentry` | The `agentry` command and its subcommands. |
| `pkg/runner` | The runner: it compiles a run plan from a release's declared capabilities, drives the Docker engine, hosts the harness adapters and the MCP gateway, decodes input files, applies the operator policy and writes the receipt. |
| `runnerimage` | The runner image: the in-container tools server, the socket filter, the network proxy, the decoders and the host link. |
| `pkg/drafter`, `pkg/examiner`, `pkg/knowledge/creator`, `pkg/voice/writer` | The built-in QuickAgents the binary embeds. |
| `pkg/trust`, `pkg/installed`, `pkg/schedule`, `pkg/tui` and the other `pkg/` packages | Key pinning, the local install store, scheduling, the terminal UI and the helpers around them. |
| `internal/` | Packages private to the CLI. |
| `scripts/` | Release, signing, image verification and acceptance scripts. |
| `build/` | Release inputs: the registry URLs a build bakes in and the Windows signing root. |
| `testdata/` | QuickAgent fixtures the runner tests use. |

## Releases

A tag is one of two forms:

- `vX.Y.Z` on `main`'s first-parent line is a production release. Its
  binaries bake `https://secureagentry.net` as the default registry.
- `vX.Y.Z-dev.N` on `dev`'s first-parent line is a prerelease. Its binaries
  bake `https://dev.secureagentry.net`.

`build/release.env` names both registries. `scripts/release-registry.sh`
picks the line from the commit's history and `scripts/release-tag.sh check`
refuses any other tag form. The `release` workflow builds the binaries with
`make dist`, signs the Windows executable and the macOS binaries when the
signing inputs are configured, writes `BUILD-INFO` and `SHA256SUMS`, and
publishes a GitHub Release. To check a download against the sums, run
`sha256sum -c --ignore-missing SHA256SUMS` beside it; `BUILD-INFO` records the
commit, the channel, the registry and the signatures. `agentry --version`
prints the version and the registry the binary falls back to. The Windows
signing setup is described in `build/windows/signing/README.md`.

## Using it

- Install and download the CLI: <https://docs.secureagentry.net/agentry/downloads/>
- Every command, the runner, the operator policy and exit codes:
  <https://docs.secureagentry.net/runner/>
- The QuickAgent package format: <https://docs.secureagentry.net/quickagents/>
- Run `agentry --help` for the command list.

To start a new QuickAgent, use the
[template](https://github.com/blaktron/agentry-template).

## Security

See [SECURITY.md](SECURITY.md) to report a vulnerability.

## Licence

MIT. See [LICENSE](LICENSE).

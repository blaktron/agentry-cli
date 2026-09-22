# agentry CLI (Agentry). Depends on the main module
# github.com/blaktron/agentry (private): builders need
#   export GOPRIVATE=github.com/blaktron/*
# and git credentials for github.com/blaktron (a PAT via credential helper or
# an url.<...>.insteadOf rewrite).
export PATH := /opt/homebrew/bin:$(PATH)
GO ?= go
LDFLAGS := -X github.com/blaktron/agentry-cli/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)

.PHONY: build test lint check clean accept-runner dist

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/agentry ./cmd/agentry

# -race: the engine and the session pump are goroutines over a docker
# process and a TUI, and §9 claims the suites run under the detector;
# they did not until agentry-cli#160.
test:
	$(GO) test -race ./...

lint:
	@fmt_out=$$(gofmt -l .); if [ -n "$$fmt_out" ]; then echo "gofmt needed on:"; echo "$$fmt_out"; exit 1; fi
	$(GO) vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; skipped"; fi

check: lint test build

# The five release binaries into dist/, named as the site's download manifest
# admits them (agentry-cli#196; plan cli-release-signing.md §4). VERSION is
# the tag (bare semver, the desktop's convention) or the describe string;
# AGENTRY_DEFAULT_REGISTRY, when set, is baked in as the fallback registry.
# Pure Go, CGO off, so every target builds on any host with the toolchain and
# nothing else — no signing input is read here (plan D5): the release
# workflow signs the Windows exe after this target, not inside it.
DIST_VERSION ?= $(shell v=$$(git describe --tags --always --dirty 2>/dev/null) || v=0.0.0-dev; echo "$$v" | sed 's/^v//')
DIST_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
dist:
	@mkdir -p dist
	@for t in $(DIST_TARGETS); do \
	  os=$${t%/*}; arch=$${t#*/}; ext=""; [ "$$os" = windows ] && ext=.exe; \
	  out="dist/agentry-$(DIST_VERSION)-$$os-$$arch$$ext"; echo "  $$out"; \
	  GOWORK=off CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
	    -ldflags "-s -w -X github.com/blaktron/agentry-cli/internal/version.Version=$(DIST_VERSION)$(if $(AGENTRY_DEFAULT_REGISTRY), -X main.defaultRegistryURL=$(AGENTRY_DEFAULT_REGISTRY),)" \
	    -o "$$out" ./cmd/agentry || exit 1; \
	done

# Runner acceptance (RUNNER-PLAN §R12): r0/r1 need no docker; r2 self-skips
# without a docker daemon + ANTHROPIC_API_KEY.
accept-runner: build
	scripts/acceptance/runner-r0.sh
	scripts/acceptance/runner-r1.sh
	scripts/acceptance/runner-r2.sh

clean:
	rm -rf bin

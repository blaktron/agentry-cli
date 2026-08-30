# agentry CLI (SecureAgentry). Depends on the main module
# github.com/blaktron/agentry (private): builders need
#   export GOPRIVATE=github.com/blaktron/*
# and git credentials for github.com/blaktron (a PAT via credential helper or
# an url.<...>.insteadOf rewrite).
export PATH := /opt/homebrew/bin:$(PATH)
GO ?= go
LDFLAGS := -X github.com/blaktron/agentry-cli/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)

.PHONY: build test lint check clean

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o bin/agentry ./cmd/agentry

test:
	$(GO) test ./...

lint:
	@fmt_out=$$(gofmt -l .); if [ -n "$$fmt_out" ]; then echo "gofmt needed on:"; echo "$$fmt_out"; exit 1; fi
	$(GO) vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; skipped"; fi

check: lint test build

clean:
	rm -rf bin

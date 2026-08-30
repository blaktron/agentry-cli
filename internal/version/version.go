// Package version carries the CLI's build-stamped version.
package version

// Version is set at build time via -ldflags
// "-X github.com/blaktron/agentry-cli/internal/version.Version=v1.2.3".
var Version = "0.0.0-dev"

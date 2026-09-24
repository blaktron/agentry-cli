// Package version carries the CLI's build-stamped version.
package version

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"sync"
)

// devVersion is what an unstamped `go build` reports.
const devVersion = "0.0.0-dev"

// Version is set at build time via -ldflags
// "-X github.com/blaktron/agentry-cli/internal/version.Version=v1.2.3".
var Version = devVersion

var (
	buildOnce sync.Once
	build     string
)

// Build names this binary for the plan cache: a compiled run plan records
// it (spec.Plan.CompiledBy), and a plan recorded by another build is
// recompiled, because the compiler's output can change while the plan
// schema does not.
//
// A stamped release is named by its version, which is unique to the commit
// it was built from. An unstamped build (0.0.0-dev) or a dirty one
// (describe's -dirty suffix) carries the same version across code changes —
// the hosted worker's CLI was built unstamped, so every worker build kept
// the plans an older CLI had cached and ran them (agentry-cli#144). Its
// name adds a digest of the running executable instead: another binary is
// another build, and the same binary (a reproducible -trimpath build of the
// same source included) is the same build. Computed once per process.
func Build() string {
	buildOnce.Do(func() { build = buildID(Version, os.Executable) })
	return build
}

// buildID is Build with its inputs injected. When the executable cannot be
// read it returns the version alone: the plan check is then what it was
// before, never stricter than it can prove.
func buildID(v string, executable func() (string, error)) string {
	if v != devVersion && !strings.HasSuffix(v, "-dirty") {
		return v
	}
	path, err := executable()
	if err != nil {
		return v
	}
	f, err := os.Open(path) //nolint:gosec // the running executable
	if err != nil {
		return v
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return v
	}
	return v + "+exe." + hex.EncodeToString(h.Sum(nil))[:16]
}

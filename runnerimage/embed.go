// Package runnerimage is the embedded build context of the locally built
// agentry-runner image. The tree leads a double life:
// in this repo it is ordinary unit-tested Go source; at first `agentry
// run` it is written out verbatim and compiled standalone inside `docker
// build`. Self-containment is the point (first use must need no
// registry of ours), so everything embedded here
// is stdlib-only, and TestEmbeddedSourcesStdlibOnly keeps it that way.
package runnerimage

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed Dockerfile main.go applink/*.go brotli/*.go brotli/LICENSE bridge/*.go beneath/*.go sockfilter/*.go sockfilter/*.json sockfilter/chromium-seccomp.LICENSE sockfilter/chromium-seccomp.NOTICE hostlink/*.go toolsrv/*.go ical/*.go toolargs/*.go toolsrv/pdf/* netproxy/*.go minimize/*.go portrelay/*.go confine/*.go decode/*.go judge/*.go websearch/*.go reportdesign/*.go reportdesign/builtins/*.json webscan/*.go probe/*.go source/*.go netaddr/*.go send/*.go
var contextFS embed.FS

// ImageTagPrefix names the locally built runner image; the engine derives
// container images from ImageTag and never pulls this name from anywhere.
const ImageTagPrefix = "agentry-runner"

// contextGoMod is the go.mod generated into the materialized context. The
// module path matches this repo so main.go's runnerimage/* imports
// resolve inside the standalone module; there are no require lines
// because the tree is stdlib-only, which is what lets the docker build
// run with no module downloads. The go line is the CLI's own, to the
// patch (TestContextGoModFollowsTheModule): under the Go images'
// GOTOOLCHAIN=local, a builder older than it refuses rather than building
// the runner with an older standard library.
const contextGoMod = "module github.com/blaktron/agentry-cli\n\ngo 1.27.3\n"

// mainIgnoreTag is the build constraint main.go carries IN THIS REPO,
// where it keeps package main out of package runnerimage's directory
// build. Materialize strips it so the standalone context compiles the
// file as the ./runnerimage main package the Dockerfile expects.
const mainIgnoreTag = "//go:build ignore\n"

// ImageTag returns the local image tag for one CLI version, e.g.
// "agentry-runner:0.3.0". A version that is empty or would not survive
// docker's tag grammar (charset [A-Za-z0-9_.-], no leading '.' or '-',
// bounded length) becomes "dev" rather than a build error at run time —
// the tag only namespaces local builds, it proves nothing.
func ImageTag(version string) string {
	if !validTagVersion(version) {
		version = "dev"
	}
	return ImageTagPrefix + ":" + version
}

func validTagVersion(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return v[0] != '.' && v[0] != '-'
}

// Materialize writes the standalone build context into dir: the embedded
// files placed under dir/runnerimage/ (Dockerfile promoted to
// dir/Dockerfile), *_test.go files skipped, plus a generated dir/go.mod
// (module github.com/blaktron/agentry-cli, the CLI's go line) so the context
// compiles with no network access beyond the base images, and a
// dir/.dockerignore excluding tests. Overwrite-safe: MkdirAll + WriteFile,
// so re-materializing over an existing context just refreshes it.
func Materialize(dir string) error {
	err := fs.WalkDir(contextFS, ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		data, err := contextFS.ReadFile(p)
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, "runnerimage", filepath.FromSlash(p))
		if p == "Dockerfile" {
			// docker build wants the Dockerfile at the context root; the
			// sources stay under runnerimage/ so the module layout — and
			// the `go build ./runnerimage` line — match this repo.
			dest = filepath.Join(dir, "Dockerfile")
		}
		if p == "main.go" {
			data = stripIgnoreConstraint(data)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("materialize build context: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(contextGoMod), 0o644); err != nil {
		return fmt.Errorf("materialize go.mod: %w", err)
	}
	// Belt and braces: Materialize already skips tests, but a context
	// someone assembled by hand-copying the repo tree should build the
	// same image this one does.
	if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("*_test.go\n"), 0o644); err != nil {
		return fmt.Errorf("materialize .dockerignore: %w", err)
	}
	return nil
}

// stripIgnoreConstraint removes main.go's repo-only build tag (and the
// blank line that separates a constraint from what follows). Lenient by
// design: if the tag ever disappears from the source, the repo build
// breaks loudly on the package collision long before this runs.
func stripIgnoreConstraint(src []byte) []byte {
	s := string(src)
	if !strings.HasPrefix(s, mainIgnoreTag) {
		return src
	}
	s = strings.TrimPrefix(s, mainIgnoreTag)
	s = strings.TrimPrefix(s, "\n")
	return []byte(s)
}

package runnerimage

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

// ContentVersion identifies the embedded build context by content: the
// first 12 hex digits of a SHA-256 over every embedded file's path and
// bytes (tests excluded, the same set Materialize writes out) and the
// generated go.mod, whose go line decides the oldest builder that may
// compile the image (agentry-cli#786). Two clients
// that embed the same context — the CLI and the desktop app at whatever
// versions — derive the same tag and therefore share one built image; a
// change to the context changes the tag and forces a rebuild. Deriving the
// tag from each client's own release version instead (D-009, agentry-cli#6)
// meant two clients on one machine built two images of identical content
// and a plan compiled by one could fail the other's gateway probe with
// exit 13.
func ContentVersion() string {
	contentVersionOnce.Do(func() {
		var paths []string
		_ = fs.WalkDir(contextFS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			paths = append(paths, p)
			return nil
		})
		sort.Strings(paths)
		h := sha256.New()
		for _, p := range paths {
			data, _ := contextFS.ReadFile(p)
			if p == "main.go" {
				data = stripIgnoreConstraint(data)
			}
			h.Write([]byte(p))
			h.Write([]byte{0})
			h.Write(data)
			h.Write([]byte{0})
		}
		h.Write([]byte("go.mod\x00" + contextGoMod + "\x00"))
		contentVersion = "c" + hex.EncodeToString(h.Sum(nil))[:12]
	})
	return contentVersion
}

var (
	contentVersionOnce sync.Once
	contentVersion     string
)

// DefaultImageTag is the tag every client builds and runs by default:
// ImageTagPrefix + ":" + ContentVersion(). A policy `images.runner` pin
// still wins (policy.RunnerImageTag).
func DefaultImageTag() string { return ImageTag(ContentVersion()) }

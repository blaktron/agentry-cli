// Package ship lays a knowledge pack into a QuickAgent's package so the
// release carries it: the pack's files under knowledge/<name>/ with the
// manifest.json the registry holds a shipped pack to, the entry in the
// package's knowledge.ships and the patterns of publish.include that put
// it in the archive, and the sidecar that records where the pack was laid
// down from.
//
// A shipped pack is part of the release. Everyone who can download the
// release can read it, so the commands that ship one ask the publisher to
// confirm it, and the confirmation names each pack with the origin laid
// down here (Entry.Origin) for the registry to check against its own
// records.
package ship

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry/pkg/manifest"
)

// The most a release ships: packs, and the size and files of each, as the
// registry holds them.
const (
	MaxPacks     = manifest.MaxShippedPacks
	MaxPackBytes = manifest.MaxKnowledgePackBytes
	MaxPackFiles = manifest.MaxKnowledgePackFiles
)

// IncludeAll is the publish.include pattern that puts every shipped pack in
// the archive.
const IncludeAll = manifest.KnowledgeDir + "/**"

// ErrPastCap is a pack past what a release may ship.
var ErrPastCap = errors.New("knowledge pack is past what a release ships")

// The kinds of store a shipped pack is laid down from.
const (
	KindOwn = "own"
	KindOrg = "org"
)

// Origin is the stored pack in a registry that a shipped pack was laid down
// from: the claim the publish confirmation carries, which the registry
// checks (the pack exists and is ready, it is the publisher's own or the
// organization's, and its digest is the shipped pack's).
type Origin struct {
	Kind   string `json:"kind"`
	Org    string `json:"org,omitempty"`
	PackID string `json:"packId"`
}

// Pack is one shipped pack as the publish confirmation names it: its name
// and, when it was laid down from a stored pack, the origin. A pack with no
// origin is the publisher's own files.
type Pack struct {
	Name   string  `json:"name"`
	Origin *Origin `json:"origin,omitempty"`
}

// Laid is a pack as it now stands under knowledge/<name>/.
type Laid struct {
	Name  string
	Pages int
	// Bytes is every file of the pack, its manifest.json included, as the
	// registry counts it against the 10 MB a pack may be.
	Bytes int64
	// Files is the files of the pack's tree, as the registry counts them
	// against the 2,000 a pack may hold.
	Files int
	// SHA256 is the tree's digest, the one in the pack's manifest.json.
	SHA256 string
}

// Dir is the pack's folder in a package.
func Dir(agentDir, name string) string {
	return filepath.Join(agentDir, manifest.KnowledgeDir, name)
}

// shippedManifest is the manifest.json a shipped pack carries: the pack's
// own counts and digest, which the registry holds to the tree beside it.
// The pack's id, its builder and any scan record stay behind: the file is
// published with the release, and a record in it would be a claim only.
func shippedManifest(m knowledge.Manifest) ([]byte, error) {
	out := knowledge.Manifest{
		Schema: knowledge.Schema, Name: m.Name, Created: m.Created, Updated: m.Updated,
		Pages: m.Pages, Bytes: m.Bytes, SHA256: m.SHA256,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Check holds a pack to what a release may ship, before anything is
// written: at most 10 MB with its manifest.json and 2,000 files.
func Check(p *knowledge.Pack) error {
	files, err := knowledge.TreeFiles(p.Dir)
	if err != nil {
		return err
	}
	mf, err := shippedManifest(p.Manifest)
	if err != nil {
		return err
	}
	return checkCaps(p.Manifest.Name, p.Manifest.Bytes+int64(len(mf)), len(files))
}

func checkCaps(name string, bytes int64, files int) error {
	if bytes > MaxPackBytes || files > MaxPackFiles {
		return fmt.Errorf("%s is %s in %d files; a release ships at most %s and %d files a pack: %w",
			name, size(bytes), files, size(MaxPackBytes), MaxPackFiles, ErrPastCap)
	}
	return nil
}

// size is a byte count as a person reads it.
func size(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// Lay puts the verified pack p under agentDir/knowledge/<name>/, replacing
// what is there: its tree, and a manifest.json the registry accepts. The
// pack is written beside its place and moved in whole, so a refusal leaves
// the folder as it was. It refuses a pack past what a release ships.
func Lay(agentDir string, p *knowledge.Pack) (Laid, error) {
	name := p.Manifest.Name
	if err := knowledge.ValidName(name); err != nil {
		return Laid{}, err
	}
	files, err := knowledge.TreeFiles(p.Dir)
	if err != nil {
		return Laid{}, err
	}
	mf, err := shippedManifest(p.Manifest)
	if err != nil {
		return Laid{}, err
	}
	total := p.Manifest.Bytes + int64(len(mf))
	if err := checkCaps(name, total, len(files)); err != nil {
		return Laid{}, err
	}
	root := filepath.Join(agentDir, manifest.KnowledgeDir)
	if fi, err := os.Lstat(root); err == nil && !fi.IsDir() {
		return Laid{}, fmt.Errorf("%s is not a folder: a package keeps its shipped packs there", root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Laid{}, err
	}
	stage, err := os.MkdirTemp(root, ".lay-")
	if err != nil {
		return Laid{}, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	staged := filepath.Join(stage, name)
	if err := knowledge.CopyPack(p.Dir, staged); err != nil {
		return Laid{}, err
	}
	if err := os.WriteFile(filepath.Join(staged, knowledge.ManifestFile), mf, 0o644); err != nil { //nolint:gosec // the publisher's own package
		return Laid{}, err
	}
	got, err := knowledge.Load(staged)
	if err != nil {
		return Laid{}, err
	}
	final := Dir(agentDir, name)
	if err := os.RemoveAll(final); err != nil {
		return Laid{}, err
	}
	if err := os.Rename(staged, final); err != nil {
		return Laid{}, err
	}
	return Laid{Name: name, Pages: got.Manifest.Pages, Bytes: total, Files: len(files), SHA256: got.Manifest.SHA256}, nil
}

// Remove takes the pack's folder out of the package. A pack that is not
// there is not an error.
func Remove(agentDir, name string) error {
	if err := knowledge.ValidName(name); err != nil {
		return err
	}
	if err := os.RemoveAll(Dir(agentDir, name)); err != nil {
		return err
	}
	// The folder that held the packs goes with the last one.
	root := filepath.Join(agentDir, manifest.KnowledgeDir)
	if entries, err := os.ReadDir(root); err == nil && len(entries) == 0 {
		_ = os.Remove(root)
	}
	return nil
}

// DirSize is the bytes and files of everything under a shipped pack's
// folder, as the archive will carry it.
func DirSize(agentDir, name string) (bytes int64, files int, err error) {
	err = filepath.WalkDir(Dir(agentDir, name), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		bytes += fi.Size()
		files++
		return nil
	})
	return bytes, files, err
}

// String names a laid pack for the person: its pages and its size.
func (l Laid) String() string {
	return fmt.Sprintf("%s: %s, %s", l.Name, knowledge.CountPages(l.Pages), size(l.Bytes))
}

// Covers says whether the include patterns put a pack's files in the
// archive: its manifest.json and its index.
func Covers(include []string, name string) bool {
	for _, rel := range []string{"manifest.json", "wiki/index.md"} {
		files := []manifest.FileInfo{{Path: path.Join(manifest.KnowledgeDir, name, rel)}}
		if len(manifest.ResolveInclude(include, files)) == 0 {
			return false
		}
	}
	return true
}

// Names are the packs the entries name, in order.
func Names(packs []Pack) []string {
	out := make([]string, 0, len(packs))
	for _, p := range packs {
		out = append(out, p.Name)
	}
	return out
}

package ship

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// RecordFile is the sidecar, relative to the package, that remembers where
// each shipped pack was laid down from. It is not part of the manifest and
// not part of the release: publish.include never names it.
const RecordFile = ".agentry/knowledge-ships.json"

// RecordSchema is the sidecar's format.
const RecordSchema = "knowledge-ships/v1"

// Entry is the sidecar's record of one shipped pack.
type Entry struct {
	Name string `json:"name"`
	// SHA256 is the tree digest that was laid down. The origin is claimed
	// only while the pack's manifest.json still says it.
	SHA256 string `json:"sha256"`
	// Registry is the registry the origin's pack id belongs to.
	Registry string  `json:"registry,omitempty"`
	Origin   *Origin `json:"origin,omitempty"`
}

// Record is the sidecar.
type Record struct {
	Schema string  `json:"schema"`
	Packs  []Entry `json:"packs"`
}

// ReadRecord reads the package's sidecar: an empty record when there is
// none.
func ReadRecord(agentDir string) (Record, error) {
	raw, err := os.ReadFile(filepath.Join(agentDir, filepath.FromSlash(RecordFile))) //nolint:gosec // the publisher's own package
	if errors.Is(err, fs.ErrNotExist) {
		return Record{Schema: RecordSchema}, nil
	}
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("%s: %w", RecordFile, err)
	}
	if r.Schema != RecordSchema {
		return Record{}, fmt.Errorf("%s: schema %q is not %q", RecordFile, r.Schema, RecordSchema)
	}
	return r, nil
}

// Set records e, replacing the entry of its name.
func (r *Record) Set(e Entry) {
	r.Remove(e.Name)
	r.Packs = append(r.Packs, e)
	slices.SortFunc(r.Packs, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
}

// Remove forgets the pack of that name.
func (r *Record) Remove(name string) {
	r.Packs = slices.DeleteFunc(r.Packs, func(e Entry) bool { return e.Name == name })
}

// Find is the entry of that name.
func (r Record) Find(name string) (Entry, bool) {
	for _, e := range r.Packs {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// WriteRecord writes the sidecar, or removes it when no pack is left.
func WriteRecord(agentDir string, r Record) error {
	p := filepath.Join(agentDir, filepath.FromSlash(RecordFile))
	if len(r.Packs) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		_ = os.Remove(filepath.Dir(p)) // only when nothing else is there
		return nil
	}
	r.Schema = RecordSchema
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644) //nolint:gosec // the publisher's own package
}

// OriginOf is the origin to claim for the shipped pack of that name when
// the package is published to the registry at reg, or why none is claimed:
// it belongs to another registry, or the pack's manifest.json is no longer
// the one that was laid down (the pack was replaced or edited by hand). A
// pack with no origin is sent as the publisher's own files, and the reason
// is empty when the sidecar never recorded one.
func OriginOf(agentDir string, r Record, name, reg string) (*Origin, string) {
	e, ok := r.Find(name)
	switch {
	case !ok || e.Origin == nil:
		return nil, ""
	case strings.TrimRight(e.Registry, "/") != strings.TrimRight(reg, "/"):
		return nil, "it was laid down from a pack at " + e.Registry + ", not at this registry"
	}
	raw, err := os.ReadFile(filepath.Join(Dir(agentDir, name), "manifest.json")) //nolint:gosec // the publisher's own package
	if err != nil {
		return nil, "its manifest.json could not be read"
	}
	var m struct {
		SHA256 string `json:"sha256"`
	}
	if json.Unmarshal(raw, &m) != nil || m.SHA256 != e.SHA256 {
		return nil, "its files are not the ones laid down from the stored pack any more"
	}
	o := *e.Origin
	return &o, ""
}

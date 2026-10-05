package engine

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry/pkg/manifest"
)

// ShippedPacks loads the knowledge packs a release ships (plan
// knowledge.md §4.6, agentry-cli#320): each knowledge/<name>/ its
// manifest's knowledge.ships names, from the release's own files in
// workspaceDir. Those are the files the archive digest and the attestation
// cover — for a built-in package, its embedded tree, which Materialize
// holds to its digest (agentry-cli#657) — so what the run mounts is what
// was verified and never re-fetched. Each pack must load as the CLI's
// format and name itself; one that does not is a verification failure,
// since the registry refuses such a release at submit (V11). release names
// the release's bytes for the receipt ("archive sha256:…"). The engine
// copies each pack into the run and verifies the copy as it does an
// attached pack.
func ShippedPacks(workspaceDir string, m *manifest.Manifest, release string) ([]AttachedPack, error) {
	var out []AttachedPack
	for _, name := range m.ShippedPacks() {
		p, err := knowledge.Load(filepath.Join(workspaceDir, manifest.KnowledgeDir, name))
		if err != nil {
			return nil, Classify(ExitVerify, fmt.Errorf("the release ships the knowledge pack %s, and it does not load from the release's files: %w", name, err))
		}
		if p.Manifest.Name != name {
			return nil, Classify(ExitVerify, fmt.Errorf("the release ships the knowledge pack %s, and its manifest.json names %s", name, p.Manifest.Name))
		}
		// A shipped pack is Agentry's to carry, so Agentry's cap holds,
		// not this computer's (agentry-cli#429): the registry refuses a
		// larger one at submit (V8), and a local package hears it here.
		if p.Manifest.Bytes > knowledge.MaxBytes {
			return nil, Classify(ExitVerify, fmt.Errorf("the release ships the knowledge pack %s at %d bytes; a shipped pack is at most %s", name, p.Manifest.Bytes, knowledge.FormatCap(knowledge.MaxBytes)))
		}
		a := AttachPack(p)
		a.Shipped = &ShippedFrom{InWorkspace: manifest.KnowledgeDir + "/" + name, Release: release}
		out = append(out, a)
	}
	return out, nil
}

// PackNameTaken is an attached pack whose name a shipped pack already
// takes: a run mounts each pack at /knowledge/<name>, so the two cannot
// both be mounted. Each caller words the refusal for its own surface.
type PackNameTaken struct{ Name string }

func (e *PackNameTaken) Error() string {
	return "the release ships a knowledge pack named " + e.Name
}

// WithShipped puts a release's shipped packs before the attached ones,
// refusing (ExitCLI, a *PackNameTaken) an attached pack that takes a
// shipped pack's name.
func WithShipped(shipped, attached []AttachedPack) ([]AttachedPack, error) {
	if len(shipped) == 0 {
		return attached, nil
	}
	names := map[string]bool{}
	for _, s := range shipped {
		names[s.Name] = true
	}
	for _, a := range attached {
		if names[a.Name] {
			return nil, Classify(ExitCLI, &PackNameTaken{Name: a.Name})
		}
	}
	return append(append([]AttachedPack(nil), shipped...), attached...), nil
}

// IsPackNameTaken reports whether err is WithShipped's refusal, and names
// the pack.
func IsPackNameTaken(err error) (string, bool) {
	var t *PackNameTaken
	if errors.As(err, &t) {
		return t.Name, true
	}
	return "", false
}

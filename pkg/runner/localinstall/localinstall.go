// Package localinstall installs a QuickAgent from a folder on this machine
// that was never published, into the runner home, where it lists among the
// installed QuickAgents and runs by name (agentry-cli#491). It is how the
// desktop's Drafter "Install" puts a draft on this computer without a
// registry, a pipeline or a credit, and `agentry install --local DIR` is
// the CLI's form of it.
//
// A local install is unsigned: nothing but the folder vouches for it, so a
// run of it asks the unsigned consent every time, as `run --local` does.
// Unlike `run --local`, it is not its own source once installed: the folder
// is staged as `agentry publish` would pack it, the consent is the staged
// manifest's, that same staged tree is laid down and sealed under the
// runner home's integrity key, and a run holds it to that record like any
// installed release. A copy whose files changed after the install is
// refused, and laid down again only from its folder, on the person's word
// (Repair).
package localinstall

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/trust"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
	"github.com/blaktron/agentry/pkg/manifest"
)

// ErrInvalidManifest wraps a folder whose manifest the registry's own
// publish rules refuse.
var ErrInvalidManifest = errors.New("the folder's manifest is invalid")

// Staged is a folder packed as publish packs it and extracted aside: the
// tree and the manifest a consent is shown for, and the only tree Install
// lays down, so what is installed is what was consented to.
type Staged struct {
	Manifest *manifest.Manifest
	// Digest is the archive digest, the same one `agentry publish` would
	// claim for this folder.
	Digest string
	// Tree is the staged package, in a directory only this user can read;
	// Close removes it.
	Tree string
	// Warnings are the manifest rules' warnings, for the caller to show.
	Warnings []error
}

// Stage packs the folder with publish.include, extracts the archive aside
// within lim, and holds the staged manifest, the one shown and installed,
// to the registry's own publish rules.
func Stage(dir string, lim pkgarchive.ExtractLimits) (*Staged, error) {
	src, err := localrun.ManifestFrom(dir)
	if err != nil {
		return nil, err
	}
	var archive bytes.Buffer
	info, err := localrun.PackWorkspace(dir, src, &archive)
	if err != nil {
		return nil, err
	}
	tree, err := os.MkdirTemp("", "agentry-local-install-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	if err := pkgarchive.Extract(bytes.NewReader(archive.Bytes()), tree, int64(archive.Len()), lim); err != nil {
		_ = os.RemoveAll(tree)
		return nil, fmt.Errorf("extract the packed folder: %w", err)
	}
	m, err := localrun.ManifestFrom(tree)
	if err != nil {
		_ = os.RemoveAll(tree)
		return nil, err
	}
	warnings, err := validate(m)
	if err != nil {
		_ = os.RemoveAll(tree)
		return nil, err
	}
	return &Staged{Manifest: m, Digest: info.Digest, Tree: tree, Warnings: warnings}, nil
}

func validate(m *manifest.Manifest) ([]error, error) {
	errs := manifest.Validate(m)
	var warnings []error
	for _, e := range errs {
		if manifest.IsWarning(e) {
			warnings = append(warnings, e)
		}
	}
	if blocking := manifest.Blocking(errs); len(blocking) > 0 {
		msgs := make([]string, 0, len(blocking))
		for _, e := range blocking {
			msgs = append(msgs, e.Error())
		}
		return nil, fmt.Errorf("%w:\n  %s", ErrInvalidManifest, strings.Join(msgs, "\n  "))
	}
	return warnings, nil
}

// Close removes the staged tree.
func (s *Staged) Close() error { return os.RemoveAll(s.Tree) }

// Ref is the installed QuickAgent's ns/name@version.
func (s *Staged) Ref() string {
	return fmt.Sprintf("%s/%s@%s", s.Manifest.Namespace, s.Manifest.Name, s.Manifest.Version)
}

// ReleaseDir is where Install lays the release down under home.
func (s *Staged) ReleaseDir(home string) string {
	return installdir.ReleaseDir(home, s.Manifest.Namespace, s.Manifest.Name, s.Manifest.Version, s.Digest)
}

// Meta is the release's install.json: the local source, and the local
// attestation state with no consent in it (install.json holds the verdict,
// never a consent).
func (s *Staged) Meta() *installdir.Meta {
	return &installdir.Meta{
		Ref: s.Ref(), Namespace: s.Manifest.Namespace, Name: s.Manifest.Name, Version: s.Manifest.Version,
		Digest: s.Digest, Source: installed.SourceLocal,
		Attestation: receipt.Attestation{State: trust.StateLocal},
	}
}

// Install lays the staged tree down under home, sealed, with the
// coding-agent export in style (installdir.StyleForHome's, as a registry
// install writes it), records it in installed.yaml as a local install of
// this digest under the runner's consent key, and removes the agent's
// other releases, as an install of a newer version does. It is called once
// the person has consented to s.Manifest's contract; it asks nothing, and
// refuses a staged tree whose manifest no longer matches what was shown.
// An installed copy of the same digest whose files changed is refused
// (*installdir.ChangedError); Repair replaces it.
func (s *Staged) Install(home, style string) (string, error) {
	return s.layDown(home, style, false)
}

// Repair is Install on the person's word that an installed copy of this
// release whose files changed is to be replaced from the folder.
func (s *Staged) Repair(home, style string) (string, error) {
	return s.layDown(home, style, true)
}

func (s *Staged) layDown(home, style string, repair bool) (string, error) {
	m := s.Manifest
	// The staged tree is this user's alone, but what is laid down is held
	// to what was shown all the same: a manifest that now asks for more
	// than the person consented to is refused (§9, the consented contract).
	now, err := localrun.ManifestFrom(s.Tree)
	if err != nil {
		return "", err
	}
	if widened := installed.ArchiveWidenings(m.Contract(), now.Contract()); len(widened) > 0 {
		return "", fmt.Errorf("the staged %s changed after it was shown, and now asks for more: %s — refused", s.Ref(), strings.Join(widened, "; "))
	}
	releaseDir := s.ReleaseDir(home)
	lay := installdir.Install
	if repair {
		lay = installdir.Repair
	}
	if err := lay(releaseDir, s.Tree, m, s.Meta(), style); err != nil {
		var changed *installdir.ChangedError
		if errors.As(err, &changed) {
			changed.Ref, changed.Repair = s.Ref(), repairCommand
		}
		return "", err
	}
	st, err := installed.Load()
	if err != nil {
		return releaseDir, fmt.Errorf("installed into %s, but its record could not be read: %w", releaseDir, err)
	}
	key := installdir.ConsentKey(home, m.Namespace, m.Name)
	st.Upsert(key, m.Version, installed.Snapshot(m.Contract()), true)
	st.MarkLocal(key, s.Digest)
	if err := installed.Save(st); err != nil {
		return releaseDir, fmt.Errorf("installed into %s, but its record could not be saved: %w", releaseDir, err)
	}
	if err := installdir.RemoveOtherVersions(installdir.AgentDir(home, m.Namespace, m.Name), releaseDir); err != nil {
		return releaseDir, fmt.Errorf("installed, but an older release could not be removed: %w", err)
	}
	return releaseDir, nil
}

// repairCommand is how a changed local install is laid down again: from
// its folder, never from a registry, which does not hold it.
const repairCommand = "agentry install --local <its folder> --allow-unsigned --repair"

// Materialize finds a local install of namespace/name under home when the
// agent's record is one, and holds it to its integrity record before
// anything trusts it. ok is false when the agent has no local install (a
// registry release, or nothing installed), or when version names another
// version than the local one: the caller resolves through the registry
// then. A local install whose files changed is refused
// (*installdir.ChangedError), and one missing or unsealed is an error: a
// local install has no registry to fetch it again from. An install record
// that cannot be read is an error with ok false, for the caller to say.
func Materialize(home, namespace, name, version string) (meta *installdir.Meta, releaseDir string, ok bool, err error) {
	st, lerr := installed.Load()
	if lerr != nil {
		return nil, "", false, lerr
	}
	rec := st.Find(installdir.ConsentKey(home, namespace, name))
	if rec == nil || !rec.Local() || rec.Digest == "" {
		return nil, "", false, nil
	}
	if version != "" && version != rec.Version {
		return nil, "", false, nil
	}
	ref := fmt.Sprintf("%s/%s@%s", namespace, name, rec.Version)
	releaseDir = installdir.ReleaseDir(home, namespace, name, rec.Version, rec.Digest)
	meta, err = installdir.Inspect(releaseDir, rec.Digest)
	var changed *installdir.ChangedError
	switch {
	case errors.As(err, &changed):
		changed.Ref, changed.Repair = ref, repairCommand
		return nil, "", true, changed
	case errors.Is(err, installdir.ErrNotInstalled), errors.Is(err, installdir.ErrNoRecord):
		return nil, "", true, fmt.Errorf("the local install of %s is missing or has no integrity record; install its folder again", ref)
	case err != nil:
		return nil, "", true, err
	}
	// Whatever install.json says, a local install runs as local: nothing
	// on disk can give it another attestation state.
	meta.Attestation = receipt.Attestation{State: trust.StateLocal}
	return meta, releaseDir, true, nil
}

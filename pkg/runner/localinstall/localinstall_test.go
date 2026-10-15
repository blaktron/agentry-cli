package localinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/installed"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/trust"
	pkgarchive "github.com/blaktron/agentry/pkg/archive"
)

var limits = pkgarchive.ExtractLimits{MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20, MaxFiles: 1000, MaxRatio: 500}

// folder writes a minimal QuickAgent package: agent.yaml, AGENT.md, and a
// scratch file publish.include leaves out.
func folder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := `schemaVersion: "2"
name: local-helper
namespace: me
modes: [autonomous]
version: 0.1.0
summary: "A helper written on this machine and never published"
license: MIT
publish:
  include: [agent.yaml, AGENT.md, LICENSE]
`
	for name, body := range map[string]string{
		"agent.yaml": manifest,
		"AGENT.md":   "Summarise the input.\n",
		"LICENSE":    "MIT\n",
		"notes.txt":  "not part of the package\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func styleOf(home string) string {
	style, _ := installdir.StyleForHome(home)
	return style
}

func setHomes(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENTRY_RUNNER_HOME", home)
	t.Setenv("AGENTRY_CONFIG_DIR", t.TempDir())
	return home
}

// A folder is staged as publish would pack it, the consent is the staged
// manifest's, and Install lays that same tree down under the runner home,
// sealed, and records it as a local install of that digest.
func TestInstallLaysTheStagedFolderDownAndRecordsItAsLocal(t *testing.T) {
	home := setHomes(t)
	s, err := Stage(folder(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Manifest.Namespace != "me" || s.Manifest.Name != "local-helper" || !strings.HasPrefix(s.Digest, "sha256:") {
		t.Fatalf("staged %+v %s", s.Manifest, s.Digest)
	}
	if _, err := os.Stat(filepath.Join(s.Tree, "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("a file publish.include leaves out was staged")
	}
	releaseDir, err := s.Install(home, styleOf(home))
	if err != nil {
		t.Fatal(err)
	}
	if releaseDir != installdir.ReleaseDir(home, "me", "local-helper", "0.1.0", s.Digest) {
		t.Fatalf("laid down at %s", releaseDir)
	}
	meta, err := installdir.Inspect(releaseDir, s.Digest)
	if err != nil {
		t.Fatalf("the install is not sealed: %v", err)
	}
	if meta.Source != installed.SourceLocal || meta.Attestation.State != trust.StateLocal || meta.Attestation.Consent != "" {
		t.Fatalf("install.json %+v", meta)
	}
	st, err := installed.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := st.Find(installdir.ConsentKey(home, "me", "local-helper"))
	if rec == nil || !rec.Local() || rec.Digest != s.Digest || rec.Version != "0.1.0" || !rec.Managed {
		t.Fatalf("record %+v", rec)
	}
}

// A run of a local install by name reads it from the runner home, never
// from a registry, and holds it to its integrity record: files changed
// after the install refuse it, as an installed registry release's do.
func TestMaterializeHoldsALocalInstallToItsRecord(t *testing.T) {
	home := setHomes(t)
	s, err := Stage(folder(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	releaseDir, err := s.Install(home, styleOf(home))
	if err != nil {
		t.Fatal(err)
	}
	meta, dir, ok, err := Materialize(home, "me", "local-helper", "")
	if err != nil || !ok || dir != releaseDir || meta.Digest != s.Digest || meta.Attestation.State != trust.StateLocal {
		t.Fatalf("materialized ok=%v dir=%s meta=%+v err=%v", ok, dir, meta, err)
	}
	if _, _, ok, err := Materialize(home, "me", "other", ""); ok || err != nil {
		t.Fatalf("no local install: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := Materialize(home, "me", "local-helper", "9.9.9"); ok || err != nil {
		t.Fatalf("a pinned version the local install is not: ok=%v err=%v, want not ours", ok, err)
	}

	if err := os.WriteFile(filepath.Join(releaseDir, "workspace", "AGENT.md"), []byte("Do something else.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err = Materialize(home, "me", "local-helper", "")
	var changed *installdir.ChangedError
	if !ok || !errors.As(err, &changed) {
		t.Fatalf("a changed local install must be refused: ok=%v err=%v", ok, err)
	}
}

// Installing the folder again after an edit replaces the record and the
// release with the new digest, and the older release directory goes.
func TestReinstallAfterAnEditReplacesTheRelease(t *testing.T) {
	home := setHomes(t)
	dir := folder(t)
	first, err := Stage(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	firstDir, err := first.Install(home, styleOf(home))
	_ = first.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("Summarise the input in one line.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Stage(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if second.Digest == first.Digest {
		t.Fatal("an edited folder staged to the same digest")
	}
	if _, err := second.Install(home, styleOf(home)); err != nil {
		t.Fatal(err)
	}
	st, _ := installed.Load()
	if rec := st.Find(installdir.ConsentKey(home, "me", "local-helper")); rec == nil || rec.Digest != second.Digest {
		t.Fatalf("record %+v, want the new digest", rec)
	}
	if _, err := os.Stat(firstDir); !os.IsNotExist(err) {
		t.Fatal("the older release directory was kept")
	}
}

// A folder whose manifest the registry's rules refuse is refused before
// anything is staged or written.
func TestStageRefusesAnInvalidManifest(t *testing.T) {
	setHomes(t)
	dir := folder(t)
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("schemaVersion: \"2\"\nnamespace: me\nname: x\nversion: not-semver\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(dir, limits); err == nil {
		t.Fatal("an invalid manifest was staged")
	}
}

// A local install whose files changed is refused with the folder as the
// way back, never a registry; Repair lays it down again from the staged
// folder, and a run finds it again.
func TestAChangedLocalInstallIsRepairedFromItsFolder(t *testing.T) {
	home := setHomes(t)
	s, err := Stage(folder(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	releaseDir, err := s.Install(home, styleOf(home))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "workspace", "AGENT.md"), []byte("Do something else.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = Materialize(home, "me", "local-helper", "")
	if err == nil || !strings.Contains(err.Error(), "--local") || strings.Contains(err.Error(), "fresh download") {
		t.Fatalf("the refusal must name the folder as the way back: %v", err)
	}
	if _, err := s.Install(home, styleOf(home)); err == nil {
		t.Fatal("an install over a changed copy of the same release was not refused")
	}
	if _, err := s.Repair(home, styleOf(home)); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if _, _, ok, err := Materialize(home, "me", "local-helper", ""); !ok || err != nil {
		t.Fatalf("after repair: ok=%v err=%v", ok, err)
	}
}

// A local install whose release directory is gone is an error that says
// to install the folder again: there is no registry to fetch it from.
func TestAMissingLocalInstallSaysToInstallAgain(t *testing.T) {
	home := setHomes(t)
	s, err := Stage(folder(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	releaseDir, err := s.Install(home, styleOf(home))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(releaseDir); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := Materialize(home, "me", "local-helper", ""); !ok || err == nil || !strings.Contains(err.Error(), "install its folder again") {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

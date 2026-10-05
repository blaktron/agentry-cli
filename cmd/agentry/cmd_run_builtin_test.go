package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// The CLI's half of TestOnlyTheEmbeddedPackagesRunAsBuiltin (plan
// knowledge.md §3; agentry-cli#318): no record on disk can make a release
// or a directory run as built in.
func TestNoCachedRecordRunsAsBuiltin(t *testing.T) {
	t.Run("a cached release claiming builtin is refused", func(t *testing.T) {
		meta := &installdir.Meta{Ref: "acme/x@1.0.0", Namespace: "acme", Name: "x", Attestation: receipt.Attestation{State: trust.StateBuiltin}}
		err := cachedAttestationGate(meta, true, true)
		if err == nil || !strings.Contains(err.Error(), "built in") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a forged record under a --local package does not change its state", func(t *testing.T) {
		isolateEnv(t)
		home, _ := isolateRunnerHome(t)
		pkg := repoPath(t, "testdata/runner/pure-text")
		meta, installDir, err := materializeLocal(stubCmd(t), pkg, home, true)
		if err != nil {
			t.Fatal(err)
		}
		forged := *meta
		forged.Attestation = receipt.Attestation{State: trust.StateBuiltin, Verified: true}
		b, _ := json.Marshal(forged)
		if err := os.WriteFile(filepath.Join(installDir, "install.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		again, _, err := materializeLocal(stubCmd(t), pkg, home, true)
		if err != nil {
			t.Fatal(err)
		}
		if again.Attestation.State != "local" || again.Attestation.Verified {
			t.Fatalf("a forged record gave the local package %+v", again.Attestation)
		}
	})
}

// TestARegistryInstallOfABuiltinIsAnOrdinaryRelease (agentry-cli#470): the
// built-ins are published in the registry under their own names, so a
// person can install agentry/drafter from it. That install is an ordinary
// release: the registry's archive digest, its own directory in the runner
// home, never the attestation state builtin. The built-in still runs from
// the copy the binary embeds, beside it and unaffected by it.
func TestARegistryInstallOfABuiltinIsAnOrdinaryRelease(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	b := localrun.Drafter()
	stage := t.TempDir()
	if err := b.Stage(stage); err != nil {
		t.Fatal(err)
	}
	// Every file of the tree, the Drafter's shipped pack under knowledge/
	// among them (agentry-cli#657).
	files := map[string]string{}
	if err := filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p) //nolint:gosec // the test's own stage
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fr := newFakeRegistry(t)
	fv := fr.addAgent(b.Namespace(), b.Name(), "autonomous", "drafts").addVersion(t, b.Version(), files)
	want, err := b.ArchiveDigest()
	if err != nil {
		t.Fatal(err)
	}
	if fv.digest != want {
		t.Fatalf("the registry's archive of the embedded tree is %s, agentry builtins says %s", fv.digest, want)
	}

	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", b.Ref(), "--allow-unsigned"); err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	installed := installdir.ReleaseDir(home, b.Namespace(), b.Name(), b.Version(), fv.digest)
	rec, ok := installdir.Load(installed, fv.digest)
	if !ok {
		t.Fatalf("no install record in %s", installed)
	}
	if rec.Attestation.State == trust.StateBuiltin || rec.Source == "builtin" {
		t.Fatalf("a registry install of %s recorded %+v", b.Ref(), rec)
	}
	if err := cachedAttestationGate(rec, true, true); err != nil && strings.Contains(err.Error(), "built in") {
		t.Fatalf("the installed release reads as built in: %v", err)
	}

	meta, dir, err := b.Materialize(home, "agentry 9.9.9 (test)")
	if err != nil {
		t.Fatal(err)
	}
	if dir == installed || meta.Attestation.State != trust.StateBuiltin || meta.Source != "builtin" {
		t.Fatalf("the built-in materialized as %+v in %s, the release is in %s", meta, dir, installed)
	}
	again, ok := installdir.Load(installed, fv.digest)
	if !ok || again.Attestation.State != "unsigned" || again.Source != "registry" || again.Digest != fv.digest {
		t.Fatalf("materializing the built-in changed the installed release: %+v", again)
	}
}

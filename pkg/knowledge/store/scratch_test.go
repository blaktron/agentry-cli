package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
)

// age sets dir's own modification time to two days ago, far past the hour
// after which the store sweeps a private directory: what a build's working
// copy looks like to the sweep once the build has run that long, since
// nothing a build writes is a direct child of it.
func age(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestABuildsWorkingCopyIsNotSweptByTheProcessThatHoldsIt (agentry-cli#507):
// a build on this computer is given an hour (DR-54), and the store sweeps
// private directories older than an hour as a dead process's leftovers.
// While the Creator runs nothing moves its working copy's own modification
// time, so past the hour any sweep took it for one and removed it under
// the build: seen live with v0.6.29, where another process's `knowledge
// import` ended a running build with "wrote no page". In one process the
// sweeps are another command's (the desktop's Download beside a build), and
// Keep's and Reseal's own, which the apply step's staging happens to spare
// by touching the directory just before them; this holds them all off
// whatever the age. What a dead process left is still swept, and a working
// copy given back is no longer held. (Another process's sweep is
// TestAHeldWorkingCopyIsKeptFreshForOtherProcesses.)
func TestABuildsWorkingCopyIsNotSweptByTheProcessThatHoldsIt(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")

	// Create: the pack is built inside a scratch, which reads two days old
	// while the build runs and when it is kept.
	scratch, done, err := a.store().Scratch()
	if err != nil {
		t.Fatal(err)
	}
	built := filepath.Join(scratch, "runbook")
	if err := os.CopyFS(built, os.DirFS(knowledgetest.Make(t, "runbook", nil).Dir)); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(a.home, store.Dir, ".open-0123456789abcdef")
	if err := os.MkdirAll(filepath.Join(dead, "runbook"), 0o700); err != nil {
		t.Fatal(err)
	}
	age(t, scratch)
	age(t, dead)
	// Mid-build, another command in this process sweeps: the desktop's
	// Download, or a second build's Scratch.
	_, otherDone, err := a.store().Scratch()
	if err != nil {
		t.Fatal(err)
	}
	otherDone()
	if !exists(built) {
		t.Fatal("another command's sweep removed a build's working copy under it")
	}
	if exists(dead) {
		t.Fatal("a directory a dead process left two days ago was not swept")
	}
	e, err := a.store().Keep(ctx, built)
	if err != nil {
		t.Fatalf("a build whose working copy reads two days old could not be kept: %v", err)
	}
	if !exists(built) {
		t.Fatal("Keep swept the working copy it was sealing")
	}
	done()
	if exists(scratch) {
		t.Fatal("a scratch given back is still there")
	}

	// Add: the kept pack is opened into a scratch, changed, and resealed,
	// its scratch reading two days old throughout.
	scratch, done, err = a.store().Scratch()
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(scratch, "runbook")
	p, err := a.store().Open(ctx, "runbook", work)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(page(t, p), "globally unique", "unique across every account", 1)
	if err := os.WriteFile(filepath.Join(work, "wiki", "topics", "buckets.md"), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := knowledge.TreeDigest(work)
	if err != nil {
		t.Fatal(err)
	}
	m := p.Manifest
	m.SHA256, m.Bytes = d.SHA256, d.Bytes
	writeManifest(t, work, m)
	age(t, scratch)
	// Any sweep in the process leaves it: another command's scratch, then
	// the reseal's own.
	_, otherDone, err = a.store().Scratch()
	if err != nil {
		t.Fatal(err)
	}
	otherDone()
	if !exists(work) {
		t.Fatal("another command's sweep removed an add's working copy under it")
	}
	got, err := a.store().Reseal(ctx, "runbook", work)
	if err != nil {
		t.Fatalf("an add whose working copy reads two days old could not be resealed: %v", err)
	}
	if got.Manifest.SHA256 == e.Manifest.SHA256 {
		t.Fatal("the reseal kept the first build's digest")
	}
	done()

	// Given back, the path is no longer held: a directory of that name left
	// behind later is swept like any other.
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	age(t, scratch)
	_, otherDone, err = a.store().Scratch()
	if err != nil {
		t.Fatal(err)
	}
	otherDone()
	if exists(scratch) {
		t.Fatal("a scratch given back is still exempt from the sweep")
	}
}

// TestARunsAttachmentIsNotHeld: the copy a run is given of an attached pack
// (OpenScratch) is copied into the run at its start, so it is left to the
// sweep as before and rests in the clear no longer than it did.
func TestARunsAttachmentIsNotHeld(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	if _, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir); err != nil {
		t.Fatal(err)
	}
	p, done, err := a.store().OpenScratch(ctx, "runbook")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	scratch := filepath.Dir(p.Dir)
	age(t, scratch)
	_, otherDone, err := a.store().Scratch()
	if err != nil {
		t.Fatal(err)
	}
	otherDone()
	if exists(scratch) {
		t.Fatal("an attachment's copy two days old was kept from the sweep")
	}
}

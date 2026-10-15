package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
)

// An organization's shared packs: kept like a person's own, their keys asked
// for at every open through the organization's route and never put in the
// keychain.

var handbook = map[string]string{
	"sources/handbook.md":     "# Handbook\nThe rota changes on Mondays.\n",
	"wiki/index.md":           "# Index\n\n- [Rota](topics/rota.md)\n",
	"wiki/log.md":             "# Log\n",
	"wiki/topics/rota.md":     "---\ntitle: Rota\n---\nThe rota changes on Mondays. [src: doc:handbook.md]\n",
	"wiki/topics/buckets.md":  "---\ntitle: Buckets\n---\nOne bucket a team.\n",
	"sources/buckets-note.md": "One bucket a team.\n",
}

func sharedMachine(t *testing.T) (*knowledgetest.Registry, *machine, string) {
	t.Helper()
	reg := knowledgetest.NewRegistry(t)
	reg.Join("acme", "amy", "member")
	id := reg.SeedShared(t, "acme", "handbook", handbook)
	return reg, newMachine(t, reg, "amy"), id
}

// keyWrites is every keychain write that names the pack's id.
func keyWrites(m *machine, id string) []string {
	m.keys.mu.Lock()
	defer m.keys.mu.Unlock()
	var out []string
	for _, a := range m.keys.sets {
		if strings.Contains(a, id) {
			out = append(out, a)
		}
	}
	return out
}

func TestASharedPacksKeyIsNeverCached(t *testing.T) {
	_, m, _ := sharedMachine(t)
	s := m.store()
	ctx := context.Background()
	e, err := s.DownloadShared(ctx, "acme", "handbook")
	if err != nil {
		t.Fatal(err)
	}
	if e.Key.Org != "acme" || e.Key.Agentry == nil {
		t.Fatalf("the kept record: %+v", e.Key)
	}
	for i := 0; i < 2; i++ {
		p, err := open(t, s, "handbook")
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if page(t, p) == "" {
			t.Fatal("the opened pack has no page")
		}
	}
	// A rewrap at sign-in moves this machine's keys, and leaves a shared
	// pack's alone.
	if _, err := s.Rewrap(ctx); err != nil {
		t.Fatal(err)
	}
	if w := keyWrites(m, e.ID()); len(w) != 0 {
		t.Fatalf("a shared pack's key reached the keychain: %v", w)
	}
	if s.Cached(e.ID()) {
		t.Fatal("a shared pack reads as cached")
	}
	if !strings.Contains(e.KeyHolder(), "acme's organization key") {
		t.Fatalf("KeyHolder = %q", e.KeyHolder())
	}
	// Signed out, it does not open: there is nothing on this machine that
	// opens it.
	m.owner = ""
	if _, err := open(t, m.store(), "handbook"); !errors.Is(err, store.ErrSignedOut) {
		t.Fatalf("a shared pack signed out: %v", err)
	}
}

func TestARemovedMemberCannotOpenASharedPack(t *testing.T) {
	reg, m, _ := sharedMachine(t)
	s := m.store()
	ctx := context.Background()
	e, err := s.DownloadShared(ctx, "acme", "handbook")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, s, "handbook"); err != nil {
		t.Fatalf("a member's open: %v", err)
	}
	reg.Remove("acme", "amy")
	_, err = open(t, s, "handbook")
	if err == nil || !errors.Is(err, store.ErrRemoteNotFound) {
		t.Fatalf("a removed member's open: %v", err)
	}
	// The sealed copy is still here, and nothing that opens it is.
	if _, err := os.Stat(filepath.Join(e.Dir, store.SealedFile)); err != nil {
		t.Fatal(err)
	}
	if w := keyWrites(m, e.ID()); len(w) != 0 {
		t.Fatalf("a shared pack's key reached the keychain: %v", w)
	}
	// Nor can they fetch another.
	if _, err := s.DownloadShared(ctx, "acme", "handbook"); err == nil {
		t.Fatal("a removed member downloaded the organization's pack")
	}
}

func TestASharedPackExportsAsTheRegistryAllows(t *testing.T) {
	reg, m, id := sharedMachine(t)
	s := m.store()
	ctx := context.Background()
	if _, err := s.DownloadShared(ctx, "acme", "handbook"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := s.Export(ctx, "handbook", dst); !errors.Is(err, store.ErrNotExportable) {
		t.Fatalf("a member's export of an unmarked pack: %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		entries, _ := os.ReadDir(dst)
		if len(entries) != 0 {
			t.Fatal("a refused export wrote files")
		}
	}
	reg.SetExportable("acme", id, true)
	p, err := s.Export(ctx, "handbook", dst)
	if err != nil {
		t.Fatalf("a member's export of a marked pack: %v", err)
	}
	if page(t, p) == "" {
		t.Fatal("the export has no page")
	}
	// An admin exports an unmarked one.
	reg.SetExportable("acme", id, false)
	reg.Join("acme", "amy", "admin")
	if _, err := s.Export(ctx, "handbook", filepath.Join(t.TempDir(), "admin")); err != nil {
		t.Fatalf("an admin's export: %v", err)
	}
}

func TestASharedPackIsNotChangedFromThisComputer(t *testing.T) {
	_, m, _ := sharedMachine(t)
	s := m.store()
	ctx := context.Background()
	if _, err := s.DownloadShared(ctx, "acme", "handbook"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Upload(ctx, "handbook"); !errors.Is(err, store.ErrShared) {
		t.Fatalf("upload of a shared pack: %v", err)
	}
	if _, err := s.Reseal(ctx, "handbook", t.TempDir()); !errors.Is(err, store.ErrShared) {
		t.Fatalf("reseal of a shared pack: %v", err)
	}
	if _, err := s.Delete(ctx, "handbook", true); !errors.Is(err, store.ErrShared) {
		t.Fatalf("delete everywhere of a shared pack: %v", err)
	}
	if _, err := s.Delete(ctx, "handbook", false); err != nil {
		t.Fatalf("delete here of a shared pack: %v", err)
	}
	if _, err := s.Find("handbook"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestASharedPackDoesNotTakeAPersonalPacksName(t *testing.T) {
	_, m, _ := sharedMachine(t)
	s := m.store()
	ctx := context.Background()
	if _, err := s.Keep(ctx, knowledgetest.Make(t, "handbook", nil).Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DownloadShared(ctx, "acme", "handbook"); !errors.Is(err, store.ErrNameTaken) {
		t.Fatalf("a shared pack over a personal one of the name: %v", err)
	}
}

// TestASharedPackWithItsOrganizationStrippedDoesNotOpen: a key.json edited
// to drop its organization sends the organization's wrapped key to the
// person's own route, which does not open it (the registry binds a wrapped
// key to its owner), so nothing opens and nothing is cached.
func TestASharedPackWithItsOrganizationStrippedDoesNotOpen(t *testing.T) {
	_, m, _ := sharedMachine(t)
	s := m.store()
	e, err := s.DownloadShared(context.Background(), "acme", "handbook")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Dir, store.KeyFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"org": "acme",`, "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Find("handbook"); got.Key.Org != "" {
		t.Fatal("the edit did not drop the organization")
	}
	if _, err := open(t, s, "handbook"); !errors.Is(err, store.ErrKeyNotYours) {
		t.Fatalf("a stripped shared pack's open: %v", err)
	}
	if w := keyWrites(m, e.ID()); len(w) != 0 {
		t.Fatalf("a shared pack's key reached the keychain: %v", w)
	}
}

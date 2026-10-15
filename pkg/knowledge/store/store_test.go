package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	"github.com/blaktron/agentry-cli/pkg/knowledge/remote"
	"github.com/blaktron/agentry-cli/pkg/knowledge/store"
)

// keychain is an in-memory OS keychain: one per machine in these tests.
type keychain struct {
	mu sync.Mutex
	m  map[string]string
	// sets is every account ever written, in order: what a test reads to
	// say nothing was put in the keychain.
	sets []string
}

func newKeychain() *keychain { return &keychain{m: map[string]string{}} }

func (k *keychain) Get(a string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[a]
	if !ok {
		return "", store.ErrNoEntry
	}
	return v, nil
}

func (k *keychain) Set(a, v string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[a] = v
	k.sets = append(k.sets, a)
	return nil
}

func (k *keychain) Delete(a string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, a)
	return nil
}

func (k *keychain) has(a string) bool {
	_, err := k.Get(a)
	return err == nil
}

// machine is one install: a runner home, its keychain, and who (if anyone)
// is signed in to the fake registry.
type machine struct {
	home  string
	keys  *keychain
	reg   *knowledgetest.Registry
	owner string // "" = signed out
}

func newMachine(t *testing.T, reg *knowledgetest.Registry, owner string) *machine {
	return &machine{home: t.TempDir(), keys: newKeychain(), reg: reg, owner: owner}
}

func (m *machine) store() *store.Store {
	return store.New(m.home, store.Options{
		Keys:    m.keys,
		Current: m.reg.URL,
		Registries: func(u string) (store.Registry, error) {
			if m.owner == "" || u != m.reg.URL {
				return nil, fmt.Errorf("%s: %w", u, store.ErrSignedOut)
			}
			return remote.New(u, m.reg.Token(m.owner), nil)
		},
		Shared: func(u, org string) (store.Shared, error) {
			if m.owner == "" || u != m.reg.URL {
				return nil, fmt.Errorf("%s: %w", u, store.ErrSignedOut)
			}
			c, err := remote.New(u, m.reg.Token(m.owner), nil)
			if err != nil {
				return nil, err
			}
			return c.Shared(org), nil
		},
	})
}

// carry copies a kept pack's files, and nothing from the keychain, to
// another machine's store: what a person copying their runner home, or a
// backup restored, would carry.
func carry(t *testing.T, e store.Entry, to *machine) {
	t.Helper()
	dst := filepath.Join(to.home, store.Dir, e.ID())
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{knowledge.ManifestFile, store.SealedFile, store.KeyFile} {
		raw, err := os.ReadFile(filepath.Join(e.Dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, f), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func open(t *testing.T, s *store.Store, name string) (*knowledge.Pack, error) {
	t.Helper()
	return s.Open(context.Background(), name, filepath.Join(t.TempDir(), "pack"))
}

func page(t *testing.T, p *knowledge.Pack) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.Dir, "wiki", "topics", "buckets.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAPackKeptOnOneInstallOpensOnAFreshOneAfterSignIn(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	src := knowledgetest.Make(t, "runbook", nil)

	e, err := a.store().Keep(ctx, src.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if e.Key.Wrapper != store.WrapperRegistry || e.Key.Registry != reg.URL || e.Key.WrappedKey.Kind != "deployment" {
		t.Fatalf("key record %+v", e.Key)
	}
	if e.ID() != src.Manifest.ID || e.Manifest.SHA256 != src.Manifest.SHA256 {
		t.Fatalf("kept as %s %s", e.ID(), e.Manifest.SHA256)
	}
	// Nothing of a page rests in the store in the clear.
	sealed, err := os.ReadFile(filepath.Join(e.Dir, store.SealedFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "globally unique") {
		t.Fatal("the sealed file holds a page's words in the clear")
	}

	t.Run("the files carried to a fresh install open after sign-in", func(t *testing.T) {
		b := newMachine(t, reg, "alice")
		carry(t, e, b)
		p, err := open(t, b.store(), "runbook")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(page(t, p), "globally unique") || p.Manifest.SHA256 != src.Manifest.SHA256 {
			t.Fatalf("opened %s", p.Manifest.SHA256)
		}
		if !b.store().Cached(e.ID()) {
			t.Fatal("the unwrapped key was not cached for the next, offline opening")
		}
	})
	t.Run("not signed out", func(t *testing.T) {
		b := newMachine(t, reg, "")
		carry(t, e, b)
		if _, err := open(t, b.store(), "runbook"); !errors.Is(err, store.ErrSignedOut) {
			t.Fatalf("err = %v, want ErrSignedOut", err)
		}
	})
	t.Run("not for another account", func(t *testing.T) {
		b := newMachine(t, reg, "bob")
		carry(t, e, b)
		if _, err := open(t, b.store(), "runbook"); !errors.Is(err, store.ErrKeyNotYours) {
			t.Fatalf("err = %v, want ErrKeyNotYours", err)
		}
	})
	t.Run("uploaded, then downloaded on a fresh install", func(t *testing.T) {
		if _, r, err := a.store().Upload(ctx, "runbook"); err != nil || r.State != store.StateReady || r.Digest != src.Manifest.SHA256 {
			t.Fatalf("upload: %+v %v", r, err)
		}
		b := newMachine(t, reg, "alice")
		got, err := b.store().Download(ctx, "runbook")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID() != e.ID() || got.Manifest.SHA256 != e.Manifest.SHA256 || !got.Manifest.Scan.Scanned || got.Key.Agentry == nil {
			t.Fatalf("downloaded %+v", got)
		}
		// The registry's record is kept as it wrote it, and read back, as
		// every record in a file is, as one this client has not confirmed:
		// nothing on disk can make a pack read as scanned.
		if d := got.Manifest.Scan.Describe(); !strings.Contains(d, "not confirmed") {
			t.Fatalf("a downloaded scan record reads %q", d)
		}
		p, err := open(t, b.store(), "runbook")
		if err != nil || !strings.Contains(page(t, p), "globally unique") {
			t.Fatalf("open the download: %v", err)
		}
		if got.Where() != "this computer and Agentry" {
			t.Fatalf("where = %q", got.Where())
		}
	})
}

func TestSignedOutAPackIsKeptUnderThisMachinesKey(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "")
	e, err := a.store().Keep(ctx, knowledgetest.Make(t, "notes", nil).Dir)
	if err != nil {
		t.Fatal(err)
	}
	if e.Key.Wrapper != store.WrapperMachine || !strings.HasPrefix(e.Key.WrappedKey.Recipient, "machine:") {
		t.Fatalf("key record %+v", e.Key)
	}
	if !strings.Contains(e.KeyHolder(), "not portable") {
		t.Fatalf("key holder %q does not say it is not portable", e.KeyHolder())
	}
	if p, err := open(t, a.store(), "notes"); err != nil || !strings.Contains(page(t, p), "globally unique") {
		t.Fatalf("open on the same machine: %v", err)
	}
	if len(reg.Requests) != 0 {
		t.Fatalf("a signed-out store reached the registry: %v", reg.Requests)
	}

	t.Run("it does not open on another machine without the machine key", func(t *testing.T) {
		b := newMachine(t, reg, "alice")
		carry(t, e, b)
		if _, err := open(t, b.store(), "notes"); err == nil || !strings.Contains(err.Error(), "machine key") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the next sign-in rewraps it to the account", func(t *testing.T) {
		a.owner = "alice"
		moved, err := a.store().Rewrap(ctx)
		if err != nil || len(moved) != 1 || moved[0] != "notes" {
			t.Fatalf("rewrap: %v %v", moved, err)
		}
		got, err := a.store().Find("notes")
		if err != nil || got.Key.Wrapper != store.WrapperRegistry {
			t.Fatalf("after the rewrap: %+v %v", got.Key, err)
		}
		b := newMachine(t, reg, "alice")
		carry(t, got, b)
		if _, err := open(t, b.store(), "notes"); err != nil {
			t.Fatalf("a rewrapped pack does not open on a fresh install: %v", err)
		}
	})
}

func TestSignedOutWithNoKeychainNothingIsKept(t *testing.T) {
	s := store.New(t.TempDir(), store.Options{})
	_, err := s.Keep(context.Background(), knowledgetest.Make(t, "notes", nil).Dir)
	if !errors.Is(err, store.ErrNoKeychain) || !strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := s.List(); len(entries) != 0 {
		t.Fatalf("kept %d packs", len(entries))
	}
}

func TestACachedKeyOpensOfflineAndDeleteRemovesIt(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	e, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !a.keys.has("knowledge:"+e.ID()) || !a.keys.has("stored:knowledge:"+e.ID()) {
		t.Fatalf("the key and its marker were not cached: %v", a.keys.m)
	}
	a.owner = "" // signed out, or the registry unreachable
	if _, err := open(t, a.store(), "runbook"); err != nil {
		t.Fatalf("a cached key does not open offline: %v", err)
	}
	if _, err := a.store().Delete(ctx, "runbook", false); err != nil {
		t.Fatal(err)
	}
	if a.keys.has("knowledge:"+e.ID()) || a.keys.has("stored:knowledge:"+e.ID()) {
		t.Fatalf("delete left the cached key: %v", a.keys.m)
	}
	if _, err := os.Stat(e.Dir); !os.IsNotExist(err) {
		t.Fatalf("delete left %s (%v)", e.Dir, err)
	}
	if _, err := a.store().Find("runbook"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("find after delete: %v", err)
	}
}

func TestResealKeepsTheKeyAndTheUploadFollows(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	e, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.store().Upload(ctx, "runbook"); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
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

	got, err := a.store().Reseal(ctx, "runbook", work)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Key.WrappedKey.Key) != string(e.Key.WrappedKey.Key) || got.Manifest.SHA256 == e.Manifest.SHA256 {
		t.Fatalf("reseal changed the key or kept the digest")
	}
	if got.Where() != "this computer and Agentry (changed here since its upload)" {
		t.Fatalf("where = %q", got.Where())
	}
	reopened, err := open(t, a.store(), "runbook")
	if err != nil || !strings.Contains(page(t, reopened), "unique across every account") {
		t.Fatalf("reopen: %v", err)
	}
	if _, r, err := a.store().Upload(ctx, "runbook"); err != nil || r.Digest != d.SHA256 {
		t.Fatalf("upload again: %+v %v", r, err)
	}
	if reg.Packs("alice") != 1 {
		t.Fatalf("the second upload made another pack in Agentry (%d)", reg.Packs("alice"))
	}
}

func TestANameIsKeptOnce(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	if _, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir); !errors.Is(err, store.ErrNameTaken) {
		t.Fatalf("err = %v", err)
	}
}

func TestATamperedPackDoesNotOpenAndLeavesNothing(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	e, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Dir, store.SealedFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-5] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "pack")
	if _, err := a.store().Open(ctx, "runbook", dst); err == nil {
		t.Fatal("a tampered pack opened")
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Fatalf("a refused opening left %d entries in %s", len(entries), dst)
	}
}

func TestDownloadWhileScanningIsRefused(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	reg.Scanning = true
	a := newMachine(t, reg, "alice")
	if _, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir); err != nil {
		t.Fatal(err)
	}
	if _, r, err := a.store().Upload(ctx, "runbook"); err != nil || r.State != store.StateScanning {
		t.Fatalf("upload: %+v %v", r, err)
	}
	b := newMachine(t, reg, "alice")
	if _, err := b.store().Download(ctx, "runbook"); !errors.Is(err, store.ErrScanning) {
		t.Fatalf("err = %v", err)
	}
	reg.Finish()
	if _, err := b.store().Download(ctx, "runbook"); err != nil {
		t.Fatalf("after the scan: %v", err)
	}
}

// The words the clients show about who holds a key follow the copy rule:
// what holds it, never a claim of safety.
func TestKeyHolderWords(t *testing.T) {
	banned := regexp.MustCompile(`(?i)\b(safe|secure|private)\b`)
	for _, e := range []store.Entry{
		{Key: store.KeyRecord{Wrapper: store.WrapperRegistry, Registry: "https://dev.secureagentry.net"}},
		{Key: store.KeyRecord{Wrapper: store.WrapperMachine}},
	} {
		w := e.KeyHolder()
		if banned.MatchString(strings.ReplaceAll(w, "secureagentry", "")) {
			t.Fatalf("%q claims more than it holds", w)
		}
	}
	reg := store.Entry{Key: store.KeyRecord{Wrapper: store.WrapperRegistry, Registry: "https://dev.secureagentry.net"}}
	if !strings.Contains(reg.KeyHolder(), "which Agentry holds") {
		t.Fatalf("%q", reg.KeyHolder())
	}
}

func writeManifest(t *testing.T, dir string, m knowledge.Manifest) {
	t.Helper()
	raw, err := jsonIndent(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, knowledge.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// One pack's key record moved beside another pack of the same machine
// does not open that pack.
func TestAKeyRecordMovedToAnotherPackDoesNotOpenIt(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "")
	one, err := a.store().Keep(ctx, knowledgetest.Make(t, "one", nil).Dir)
	if err != nil {
		t.Fatal(err)
	}
	two, err := a.store().Keep(ctx, knowledgetest.Make(t, "two", nil).Dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(one.Dir, store.KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(two.Dir, store.KeyFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, a.store(), "two"); err == nil {
		t.Fatal("pack one's machine-wrapped key opened pack two")
	}
}

func TestAPackPastTheFileCapIsNotKept(t *testing.T) {
	files := map[string]string{}
	for k, v := range knowledgetest.DefaultFiles {
		files[k] = v
	}
	for i := 0; i < knowledge.MaxFiles; i++ {
		files[fmt.Sprintf("sources/extra/f%04d.txt", i)] = "x"
	}
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	_, err := a.store().Keep(context.Background(), knowledgetest.Make(t, "big", files).Dir)
	if err == nil || !strings.Contains(err.Error(), "files") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := a.store().List(); len(entries) != 0 {
		t.Fatalf("kept %d packs", len(entries))
	}
}

func TestADownloadNeverLosesWhatIsKeptHere(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	if _, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.store().Upload(ctx, "runbook"); err != nil {
		t.Fatal(err)
	}
	// Unchanged since the upload: the download replaces it.
	if _, err := a.store().Download(ctx, "runbook"); err != nil {
		t.Fatalf("an unchanged pack was not replaced by its own copy: %v", err)
	}
	// Changed here since: refused, and the change is still there.
	work := filepath.Join(t.TempDir(), "work")
	p, err := a.store().Open(ctx, "runbook", work)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "wiki", "topics", "buckets.md"), []byte(strings.Replace(page(t, p), "globally unique", "changed here", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := knowledge.TreeDigest(work)
	if err != nil {
		t.Fatal(err)
	}
	m := p.Manifest
	m.SHA256, m.Bytes = d.SHA256, d.Bytes
	writeManifest(t, work, m)
	if _, err := a.store().Reseal(ctx, "runbook", work); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store().Download(ctx, "runbook"); err == nil || !strings.Contains(err.Error(), "changed on this computer") {
		t.Fatalf("err = %v", err)
	}
	if got, err := open(t, a.store(), "runbook"); err != nil || !strings.Contains(page(t, got), "changed here") {
		t.Fatalf("the change was lost: %v", err)
	}
	// A different pack under the name is refused too.
	b := newMachine(t, reg, "alice")
	if _, err := b.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store().Download(ctx, "runbook"); !errors.Is(err, store.ErrNameTaken) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeleteEverywhereRemovesAgentrysCopyFirst(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	if _, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.store().Upload(ctx, "runbook"); err != nil {
		t.Fatal(err)
	}
	a.owner = ""
	if _, err := a.store().Delete(ctx, "runbook", true); !errors.Is(err, store.ErrSignedOut) {
		t.Fatalf("signed out: err = %v", err)
	}
	if _, err := a.store().Find("runbook"); err != nil {
		t.Fatal("a refused delete everywhere removed the pack here")
	}
	a.owner = "alice"
	if _, err := a.store().Delete(ctx, "runbook", true); err != nil {
		t.Fatal(err)
	}
	if reg.Packs("alice") != 0 {
		t.Fatal("Agentry's copy was kept")
	}
}

// A pack past Agentry's cap is kept and opened here, and its upload is
// refused before anything is sent: it stays on this computer.
func TestAPackPastAgentrysCapStaysHere(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	dir := knowledgetest.Make(t, "archive", nil).Dir
	if err := os.WriteFile(filepath.Join(dir, "sources", "big.txt"), []byte(strings.Repeat("x", knowledge.MaxBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := knowledge.TreeDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, knowledge.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m knowledge.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.SHA256, m.Bytes = d.SHA256, d.Bytes
	writeManifest(t, dir, m)
	p, err := knowledge.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store().Keep(ctx, dir); err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if got, err := open(t, a.store(), "archive"); err != nil || got.Manifest.Bytes != p.Manifest.Bytes {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := a.store().Upload(ctx, "archive"); !errors.Is(err, store.ErrPastAgentryCap) || !strings.Contains(err.Error(), "stays on this computer") {
		t.Fatalf("upload: %v", err)
	}
	if n := reg.Packs("alice"); n != 0 {
		t.Fatalf("a refused upload reached the registry: %d packs", n)
	}
	// Extended here, it is resealed under the key it had.
	work := filepath.Join(t.TempDir(), "work")
	op, err := a.store().Open(ctx, "archive", work)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "sources", "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2, err := knowledge.TreeDigest(work)
	if err != nil {
		t.Fatal(err)
	}
	m2 := op.Manifest
	m2.SHA256, m2.Bytes = d2.SHA256, d2.Bytes
	writeManifest(t, work, m2)
	if got, err := a.store().Reseal(ctx, "archive", work); err != nil || got.Manifest.Bytes <= knowledge.MaxBytes {
		t.Fatalf("reseal: %v", err)
	}
}

// TestAgentryScanIsConfirmedWithTheRegistry: a kept pack's scan in Agentry
// is the registry's record, asked now, only while Agentry's copy is this
// pack's tree; anything else says why it could not be confirmed, and a
// pack kept here only has nothing to confirm.
func TestAgentryScanIsConfirmedWithTheRegistry(t *testing.T) {
	ctx := context.Background()
	reg := knowledgetest.NewRegistry(t)
	a := newMachine(t, reg, "alice")
	e, err := a.store().Keep(ctx, knowledgetest.Make(t, "runbook", nil).Dir)
	if err != nil {
		t.Fatal(err)
	}
	digest := e.Manifest.SHA256
	if rec, err := a.store().AgentryScan(ctx, "runbook", digest); rec != nil || err != nil {
		t.Fatalf("kept here only: %s %v", rec, err)
	}
	reg.Scanning = true
	if _, _, err := a.store().Upload(ctx, "runbook"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store().AgentryScan(ctx, "runbook", digest); !errors.Is(err, store.ErrScanUnconfirmed) || !strings.Contains(err.Error(), `"scanning"`) {
		t.Fatalf("while scanning: %v", err)
	}
	reg.Finish()
	rec, err := a.store().AgentryScan(ctx, "runbook", digest)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if json.Unmarshal(rec, &report) != nil || report["findings"] == nil {
		t.Fatalf("record %s", rec)
	}
	if _, err := a.store().AgentryScan(ctx, "runbook", "sha256:another"); !errors.Is(err, store.ErrScanUnconfirmed) {
		t.Fatalf("another tree: %v", err)
	}
	a.owner = ""
	if _, err := a.store().AgentryScan(ctx, "runbook", digest); !errors.Is(err, store.ErrScanUnconfirmed) || !errors.Is(err, store.ErrSignedOut) {
		t.Fatalf("signed out: %v", err)
	}
	a.owner = "alice"
	// Changed here since its upload.
	p, err := open(t, a.store(), "runbook")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Dir, "sources", "guide.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := knowledge.WriteIndex(p.Dir, "runbook"); err != nil {
		t.Fatal(err)
	}
	d, _ := knowledge.TreeDigest(p.Dir)
	m := p.Manifest
	m.Bytes, m.SHA256 = d.Bytes, d.SHA256
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(p.Dir, knowledge.ManifestFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	e2, err := a.store().Reseal(ctx, "runbook", p.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store().AgentryScan(ctx, "runbook", e2.Manifest.SHA256); !errors.Is(err, store.ErrScanUnconfirmed) || !strings.Contains(err.Error(), "changed here") {
		t.Fatalf("changed here: %v", err)
	}
}

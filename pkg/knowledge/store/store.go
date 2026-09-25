package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/packseal"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
)

// Options are what a Store is given rather than finds for itself.
type Options struct {
	// Keys is the OS keychain (nil: no cache and no machine key).
	Keys Keychain
	// Registries resolves a signed-in client for a registry's URL (nil:
	// every registry is signed out).
	Registries Registries
	// Current is the registry new packs are wrapped by and uploaded to:
	// the one the person is signed in to. Signed out of it (or none), a
	// new pack is wrapped by this machine's key.
	Current string
}

// Store is the local knowledge store under one runner home.
type Store struct {
	dir  string
	opts Options
}

// New is the store under the runner home.
func New(home string, opts Options) *Store {
	return &Store{dir: filepath.Join(home, Dir), opts: opts}
}

// Entry is one pack the store keeps.
type Entry struct {
	// Dir is the pack's directory in the store.
	Dir      string
	Manifest knowledge.Manifest
	Key      KeyRecord
}

// Name is the pack's name.
func (e Entry) Name() string { return e.Manifest.Name }

// ID is the pack's id.
func (e Entry) ID() string { return e.Manifest.ID }

// Where says where the pack is kept: this computer, or this computer and
// Agentry, and whether it changed here since its upload.
func (e Entry) Where() string {
	switch {
	case e.Key.Agentry == nil:
		return "this computer"
	case e.Key.Agentry.Digest != e.Manifest.SHA256:
		return "this computer and Agentry (changed here since its upload)"
	}
	return "this computer and Agentry"
}

// KeyHolder says what the pack's data key is wrapped to, in the words the
// clients show (plan §3: never "safe", "secure" or "private").
func (e Entry) KeyHolder() string {
	if e.Key.Wrapper == WrapperMachine {
		return "this computer's key, in its keychain (not portable; rewrapped to your account at your next sign-in)"
	}
	return "your account's key at " + hostOf(e.Key.Registry) + ", which Agentry holds"
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// List is every pack the store keeps, by name. An entry that cannot be
// read is left out and named in the error, beside the entries that can.
func (s *Store) List() ([]Entry, error) {
	dirs, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	var errs []error
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		e, err := s.read(filepath.Join(s.dir, d.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("knowledge store %s: %w", d.Name(), err))
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, errors.Join(errs...)
}

// read is the entry in dir, held to its shape.
func (s *Store) read(dir string) (Entry, error) {
	var e Entry
	e.Dir = dir
	raw, err := os.ReadFile(filepath.Join(dir, knowledge.ManifestFile)) //nolint:gosec // an entry of the store
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e.Manifest); err != nil {
		return e, fmt.Errorf("%s: %w", knowledge.ManifestFile, err)
	}
	if e.Manifest.Schema != knowledge.Schema || knowledge.ValidName(e.Manifest.Name) != nil || !validID(e.Manifest.ID) || filepath.Base(dir) != e.Manifest.ID {
		return e, fmt.Errorf("%s does not describe a pack kept here", knowledge.ManifestFile)
	}
	raw, err = os.ReadFile(filepath.Join(dir, KeyFile)) //nolint:gosec // an entry of the store
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e.Key); err != nil {
		return e, fmt.Errorf("%s: %w", KeyFile, err)
	}
	if e.Key.Schema != KeySchema || (e.Key.Wrapper != WrapperRegistry && e.Key.Wrapper != WrapperMachine) {
		return e, fmt.Errorf("%s is not a %s record", KeyFile, KeySchema)
	}
	return e, nil
}

// Find is the pack named name.
func (s *Store) Find(name string) (Entry, error) {
	entries, err := s.List()
	for _, e := range entries {
		if e.Name() == name {
			return e, nil
		}
	}
	if err != nil {
		return Entry{}, fmt.Errorf("knowledge pack %s: %w (%w)", name, ErrNotFound, err)
	}
	return Entry{}, fmt.Errorf("knowledge pack %s: %w", name, ErrNotFound)
}

// current is the signed-in client of the Current registry.
func (s *Store) current() (Registry, error) {
	if s.opts.Current == "" {
		return nil, ErrSignedOut
	}
	return s.registry(s.opts.Current)
}

// registry is the signed-in client of the registry at u.
func (s *Store) registry(u string) (Registry, error) {
	if s.opts.Registries == nil {
		return nil, fmt.Errorf("%s: %w", u, ErrSignedOut)
	}
	return s.opts.Registries(u)
}

// Keep seals the pack at packDir into the store under a new data key,
// wrapped by the current registry, or signed out by this machine's key. A
// pack with no id is given one. The pack directory itself is not changed.
func (s *Store) Keep(ctx context.Context, packDir string) (Entry, error) {
	s.sweep()
	p, err := knowledge.Load(packDir)
	if err != nil {
		return Entry{}, err
	}
	m := p.Manifest
	if m.ID == "" {
		if m.ID, err = newID(); err != nil {
			return Entry{}, err
		}
	}
	if !validID(m.ID) {
		return Entry{}, fmt.Errorf("knowledge pack %s: id %q is not kp_ and 32 hex digits", m.Name, m.ID)
	}
	if err := s.free(m.Name, m.ID); err != nil {
		return Entry{}, err
	}
	dataKey, err := packseal.NewKey()
	if err != nil {
		return Entry{}, err
	}
	kr, err := s.wrapNew(ctx, m.ID, dataKey)
	if err != nil {
		return Entry{}, fmt.Errorf("knowledge pack %s: %w", m.Name, err)
	}
	e, err := s.install(m, kr, func(w io.Writer) error { return sealDir(w, packDir, m, dataKey) }, "")
	if err != nil {
		return Entry{}, fmt.Errorf("knowledge pack %s: keep it: %w", m.Name, err)
	}
	if kr.Wrapper == WrapperRegistry {
		s.cache(m.ID, dataKey)
	}
	return e, nil
}

// free refuses a name or an id the store already keeps.
func (s *Store) free(name, id string) error {
	// An entry that cannot be read is left out of the listing; its id
	// still holds its directory, checked below.
	entries, _ := s.List()
	for _, e := range entries {
		switch {
		case e.Name() == name:
			return fmt.Errorf("knowledge pack %s: %w", name, ErrNameTaken)
		case e.ID() == id:
			return fmt.Errorf("knowledge pack %s: %w (as %s)", name, ErrNameTaken, e.Name())
		}
	}
	if _, err := os.Lstat(filepath.Join(s.dir, id)); err == nil {
		return fmt.Errorf("knowledge pack %s: %w (its id %s is already in the store)", name, ErrNameTaken, id)
	}
	return nil
}

// wrapNew wraps a new pack's data key: by the current registry if one is
// signed in, else by this machine's key.
func (s *Store) wrapNew(ctx context.Context, id string, dataKey []byte) (KeyRecord, error) {
	reg, err := s.current()
	switch {
	case err == nil:
		wk, err := reg.Wrap(ctx, dataKey)
		if err != nil {
			return KeyRecord{}, fmt.Errorf("wrap its key at %s: %w", reg.URL(), err)
		}
		return KeyRecord{Schema: KeySchema, Wrapper: WrapperRegistry, Registry: reg.URL(), WrappedKey: wk}, nil
	case !errors.Is(err, ErrSignedOut):
		return KeyRecord{}, err
	}
	wk, err := s.machineWrap(id, dataKey)
	if err != nil {
		return KeyRecord{}, fmt.Errorf("not signed in, and this computer's key could not be used: %w; sign in (`agentry login`) so the registry wraps it", err)
	}
	return KeyRecord{Schema: KeySchema, Wrapper: WrapperMachine, WrappedKey: wk}, nil
}

// sealDir writes the pack at dir sealed under dataKey, and checks the tree
// it sealed is the one m names (a pack changed after it was loaded is
// refused, not kept under the wrong digest).
func sealDir(w io.Writer, dir string, m knowledge.Manifest, dataKey []byte) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		d, err := knowledge.WriteTree(dir, pw)
		switch {
		case err != nil:
		case d.Files > knowledge.MaxFiles:
			// The registry refuses such a tree, and ExtractTree would
			// refuse to open it again, so it is never kept.
			err = fmt.Errorf("knowledge pack %s holds %d files, past %d", m.Name, d.Files, knowledge.MaxFiles)
		case d.SHA256 != m.SHA256:
			err = fmt.Errorf("knowledge pack %s changed while it was sealed", m.Name)
		}
		_ = pw.CloseWithError(err)
		done <- err
	}()
	err := packseal.Seal(w, dataKey, packseal.Header{Pack: m.ID, Digest: m.SHA256}, pr)
	_ = pr.CloseWithError(err)
	if werr := <-done; werr != nil {
		return werr
	}
	return err
}

// install writes an entry through a staging directory and moves it into
// place in one rename; replacing names an entry directory it takes the
// place of.
func (s *Store) install(m knowledge.Manifest, kr KeyRecord, seal func(io.Writer) error, replacing string) (Entry, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Entry{}, err
	}
	stage, err := s.stageDir()
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	f, err := os.OpenFile(filepath.Join(stage, SealedFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the store's own staging directory
	if err != nil {
		return Entry{}, err
	}
	err = seal(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Entry{}, err
	}
	if err := writeJSON(filepath.Join(stage, knowledge.ManifestFile), m); err != nil {
		return Entry{}, err
	}
	if err := writeJSON(filepath.Join(stage, KeyFile), kr); err != nil {
		return Entry{}, err
	}
	final := filepath.Join(s.dir, m.ID)
	if replacing != "" {
		old := stage + ".old"
		if err := os.Rename(replacing, old); err != nil {
			return Entry{}, err
		}
		if err := os.Rename(stage, final); err != nil {
			_ = os.Rename(old, replacing)
			return Entry{}, err
		}
		_ = os.RemoveAll(old)
	} else if err := os.Rename(stage, final); err != nil {
		return Entry{}, err
	}
	return Entry{Dir: final, Manifest: m, Key: kr}, nil
}

// stageDir is a fresh, private directory inside the store.
func (s *Store) stageDir() (string, error) { return s.privateDir(".stage-") }

// privateDir is a fresh directory inside the store, 0700, named prefix and
// a random suffix; List skips it, and sweep removes one left behind.
func (s *Store) privateDir(prefix string) (string, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	dir := filepath.Join(s.dir, prefix+hex.EncodeToString(b))
	return dir, os.Mkdir(dir, 0o700)
}

// staleAfter is how long a private directory may outlive the process that
// made it before sweep removes it: a run's attachment is copied into the
// run long before, and a build's working copy is touched as it goes.
const staleAfter = time.Hour

// sweep removes private directories a process that died left behind.
func (s *Store) sweep() {
	dirs, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() || (!strings.HasPrefix(d.Name(), ".stage-") && !strings.HasPrefix(d.Name(), ".open-")) {
			continue
		}
		if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) > staleAfter {
			_ = os.RemoveAll(filepath.Join(s.dir, d.Name()))
		}
	}
}

// Scratch is a fresh 0700 directory inside the store for a pack in the
// clear while something works on it (the Creator adding to a kept pack),
// and the function that removes it.
func (s *Store) Scratch() (dir string, done func(), err error) {
	s.sweep()
	if dir, err = s.privateDir(".open-"); err != nil {
		return "", func() {}, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// OpenScratch opens the pack named name into a private directory inside
// the store, for a run to copy into its own directory, and returns it with
// the function that removes it. The pack's pages rest in the clear there
// only until done is called (a run calls it at teardown).
func (s *Store) OpenScratch(ctx context.Context, name string) (p *knowledge.Pack, done func(), err error) {
	dir, done, err := s.Scratch()
	if err != nil {
		return nil, done, err
	}
	if p, err = s.Open(ctx, name, filepath.Join(dir, name)); err != nil {
		done()
		return nil, func() {}, err
	}
	return p, done, nil
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// dataKey is e's data key: the keychain's cached copy, else unwrapped by
// the registry that wrapped it (and cached), else by this machine's key.
// fromCache says which, so a cached key that fails to open is dropped and
// the pack asked for again.
func (s *Store) dataKey(ctx context.Context, e Entry, skipCache bool) (key []byte, fromCache bool, err error) {
	if !skipCache {
		if k, ok := s.cached(e.ID()); ok {
			return k, true, nil
		}
	}
	switch e.Key.Wrapper {
	case WrapperMachine:
		k, err := s.machineUnwrap(e.ID(), e.Key.WrappedKey)
		if err != nil {
			return nil, false, fmt.Errorf("knowledge pack %s: %w", e.Name(), err)
		}
		return k, false, nil
	case WrapperRegistry:
		reg, err := s.registry(e.Key.Registry)
		if err != nil {
			return nil, false, fmt.Errorf("knowledge pack %s: its key is wrapped by %s: %w", e.Name(), e.Key.Registry, err)
		}
		k, err := reg.Unwrap(ctx, e.Key.WrappedKey)
		if err != nil {
			return nil, false, fmt.Errorf("knowledge pack %s: unwrap its key at %s: %w", e.Name(), e.Key.Registry, err)
		}
		if len(k) != packseal.KeySize {
			return nil, false, fmt.Errorf("knowledge pack %s: %s answered a key of %d bytes", e.Name(), e.Key.Registry, len(k))
		}
		s.cache(e.ID(), k)
		return k, false, nil
	}
	return nil, false, fmt.Errorf("knowledge pack %s: its key is wrapped by %q, which this client does not know", e.Name(), e.Key.Wrapper)
}

// Open writes the pack named name, in the clear, into dst (new, or an
// empty directory) and returns it loaded and verified. This is the one way
// a kept pack's pages leave the store: into a run's copy, or an export.
func (s *Store) Open(ctx context.Context, name, dst string) (*knowledge.Pack, error) {
	e, err := s.Find(name)
	if err != nil {
		return nil, err
	}
	return s.openEntry(ctx, e, dst)
}

func (s *Store) openEntry(ctx context.Context, e Entry, dst string) (*knowledge.Pack, error) {
	sealed, err := os.ReadFile(filepath.Join(e.Dir, SealedFile)) //nolint:gosec // an entry of the store
	if err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", e.Name(), err)
	}
	key, fromCache, err := s.dataKey(ctx, e, false)
	if err != nil {
		return nil, err
	}
	p, err := s.openWith(e, sealed, key, dst)
	if err != nil && fromCache && errors.Is(err, packseal.ErrOpen) {
		// A cached key that no longer opens the pack (the pack was
		// replaced by a download under another key) is dropped, and the
		// pack asked for through its wrapper.
		_ = s.uncache(e.ID())
		if key, _, err = s.dataKey(ctx, e, true); err != nil {
			return nil, err
		}
		p, err = s.openWith(e, sealed, key, dst)
	}
	return p, err
}

func (s *Store) openWith(e Entry, sealed, key []byte, dst string) (*knowledge.Pack, error) {
	h, plain, err := packseal.Open(bytes.NewReader(sealed), key)
	if err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", e.Name(), err)
	}
	if h.Pack != e.ID() || h.Digest != e.Manifest.SHA256 {
		return nil, fmt.Errorf("knowledge pack %s: its sealed tree is pack %s with digest %s, not the %s and %s its manifest names", e.Name(), h.Pack, h.Digest, e.ID(), e.Manifest.SHA256)
	}
	if _, err := knowledge.ExtractTree(plain, dst, h.Digest); err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", e.Name(), err)
	}
	if err := writeJSON(filepath.Join(dst, knowledge.ManifestFile), e.Manifest); err != nil {
		clearDir(dst)
		return nil, err
	}
	p, err := knowledge.Load(dst)
	if err != nil {
		clearDir(dst)
		return nil, err
	}
	return p, nil
}

// clearDir empties a directory Open wrote into, when the pack it wrote
// turns out not to load.
func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// Reseal replaces the tree of the kept pack name with the pack at packDir
// (an extended copy of it: the same id or none, the same name), under the
// same data key, so its wrapped key, and a copy the registry keeps, still
// fit.
func (s *Store) Reseal(ctx context.Context, name, packDir string) (Entry, error) {
	s.sweep()
	e, err := s.Find(name)
	if err != nil {
		return Entry{}, err
	}
	p, err := knowledge.Load(packDir)
	if err != nil {
		return Entry{}, err
	}
	m := p.Manifest
	if m.Name != e.Name() || (m.ID != "" && m.ID != e.ID()) {
		return Entry{}, fmt.Errorf("knowledge pack %s: %s holds pack %s (%s), not this one", name, packDir, m.Name, m.ID)
	}
	m.ID = e.ID()
	key, _, err := s.dataKey(ctx, e, false)
	if err != nil {
		return Entry{}, err
	}
	re, err := s.install(m, e.Key, func(w io.Writer) error { return sealDir(w, packDir, m, key) }, e.Dir)
	if err != nil {
		return Entry{}, fmt.Errorf("knowledge pack %s: keep the new tree: %w", name, err)
	}
	return re, nil
}

// Delete removes the kept pack name and its cached key; with everywhere,
// the copy Agentry keeps as well, first, so a refusal there leaves the
// pack here. A copy Agentry no longer has is not an error.
func (s *Store) Delete(ctx context.Context, name string, everywhere bool) (Entry, error) {
	e, err := s.Find(name)
	if err != nil {
		return Entry{}, err
	}
	if a := e.Key.Agentry; everywhere && a != nil {
		reg, err := s.registry(a.Registry)
		if err != nil {
			return Entry{}, fmt.Errorf("knowledge pack %s: delete Agentry's copy: %w", name, err)
		}
		if err := reg.Delete(ctx, a.ID); err != nil && !errors.Is(err, ErrRemoteNotFound) {
			return Entry{}, fmt.Errorf("knowledge pack %s: delete Agentry's copy: %w", name, err)
		}
	}
	// Moved aside in one rename, then removed: an interrupted delete
	// leaves a dot-named directory the sweep finishes, never half a pack.
	trash, err := s.privateDir(".stage-")
	if err == nil {
		err = os.Remove(trash)
	}
	if err == nil {
		err = os.Rename(e.Dir, trash)
	}
	if err == nil {
		err = os.RemoveAll(trash)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("knowledge pack %s: delete it: %w", name, err)
	}
	if err := s.uncache(e.ID()); err != nil {
		return e, fmt.Errorf("knowledge pack %s was deleted, but its cached key would not leave the OS keychain: %w", name, err)
	}
	return e, nil
}

// Rewrap moves every pack wrapped by this machine's key to the current
// registry's wrapper (plan D20: at the next sign-in), and answers their
// names. Signed out, it does nothing.
func (s *Store) Rewrap(ctx context.Context) ([]string, error) {
	reg, err := s.current()
	if errors.Is(err, ErrSignedOut) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, lerr := s.List()
	var moved []string
	for _, e := range entries {
		if e.Key.Wrapper != WrapperMachine {
			continue
		}
		if err := s.rewrapOne(ctx, reg, e); err != nil {
			return moved, err
		}
		moved = append(moved, e.Name())
	}
	return moved, lerr
}

func (s *Store) rewrapOne(ctx context.Context, reg Registry, e Entry) error {
	key, err := s.machineUnwrap(e.ID(), e.Key.WrappedKey)
	if err != nil {
		return fmt.Errorf("knowledge pack %s: %w", e.Name(), err)
	}
	wk, err := reg.Wrap(ctx, key)
	if err != nil {
		return fmt.Errorf("knowledge pack %s: wrap its key at %s: %w", e.Name(), reg.URL(), err)
	}
	kr := KeyRecord{Schema: KeySchema, Wrapper: WrapperRegistry, Registry: reg.URL(), WrappedKey: wk, Agentry: e.Key.Agentry}
	if err := writeKeyAtomic(e.Dir, kr); err != nil {
		return err
	}
	s.cache(e.ID(), key)
	return nil
}

// writeKeyAtomic replaces an entry's key.json in one rename.
func writeKeyAtomic(dir string, kr KeyRecord) error {
	tmp := filepath.Join(dir, "."+KeyFile+".new")
	if err := writeJSON(tmp, kr); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, KeyFile))
}

// Upload sends the kept pack name to the current registry, sealed and
// with its wrapped key as they are, and records the registry's copy. A
// pack wrapped by this machine's key is rewrapped first. The registry
// scans what it receives: the answer is the pack as it stands there.
func (s *Store) Upload(ctx context.Context, name string) (Entry, Remote, error) {
	reg, err := s.current()
	if err != nil {
		return Entry{}, Remote{}, fmt.Errorf("upload knowledge pack %s: %w", name, err)
	}
	e, err := s.Find(name)
	if err != nil {
		return Entry{}, Remote{}, err
	}
	if e.Key.Wrapper == WrapperMachine {
		if err := s.rewrapOne(ctx, reg, e); err != nil {
			return Entry{}, Remote{}, err
		}
		if e, err = s.Find(name); err != nil {
			return Entry{}, Remote{}, err
		}
	}
	if e.Key.Registry != reg.URL() {
		return Entry{}, Remote{}, fmt.Errorf("upload knowledge pack %s: its key is wrapped by %s, not %s; upload it there", name, e.Key.Registry, reg.URL())
	}
	sealed, err := os.ReadFile(filepath.Join(e.Dir, SealedFile)) //nolint:gosec // an entry of the store
	if err != nil {
		return Entry{}, Remote{}, fmt.Errorf("upload knowledge pack %s: %w", name, err)
	}
	var r Remote
	uploaded := false
	if a := e.Key.Agentry; a != nil && a.Registry == reg.URL() {
		r, err = reg.PutTree(ctx, a.ID, sealed)
		switch {
		case err == nil:
			uploaded = true
		case !errors.Is(err, ErrRemoteNotFound):
			return Entry{}, Remote{}, fmt.Errorf("upload knowledge pack %s: %w", name, err)
		}
	}
	if !uploaded {
		created, err := reg.Create(ctx, e.Name(), e.Key.WrappedKey)
		if err != nil {
			return Entry{}, Remote{}, fmt.Errorf("upload knowledge pack %s: %w", name, err)
		}
		if r, err = reg.PutTree(ctx, created.ID, sealed); err != nil {
			return Entry{}, Remote{}, fmt.Errorf("upload knowledge pack %s: %w", name, err)
		}
	}
	e.Key.Agentry = &AgentryCopy{Registry: reg.URL(), ID: r.ID, Digest: e.Manifest.SHA256}
	if err := writeKeyAtomic(e.Dir, e.Key); err != nil {
		return Entry{}, Remote{}, fmt.Errorf("knowledge pack %s was uploaded, but its record of Agentry's copy could not be written: %w", name, err)
	}
	return e, r, nil
}

// Download keeps the pack named name from the current registry: its sealed
// tree as the registry holds it, opened with the key the registry unwraps
// and checked in full, then kept here under that wrapped key. A pack kept
// here with the same id is replaced; another pack of that name is not.
func (s *Store) Download(ctx context.Context, name string) (Entry, error) {
	s.sweep()
	reg, err := s.current()
	if err != nil {
		return Entry{}, fmt.Errorf("download knowledge pack %s: %w", name, err)
	}
	remotes, err := reg.List(ctx)
	if err != nil {
		return Entry{}, fmt.Errorf("download knowledge pack %s: %w", name, err)
	}
	var r Remote
	found := false
	for _, x := range remotes {
		if x.Name == name {
			r, found = x, true
			break
		}
	}
	switch {
	case !found:
		return Entry{}, fmt.Errorf("download knowledge pack %s: %w", name, ErrRemoteNotFound)
	case r.State == StateScanning:
		return Entry{}, fmt.Errorf("download knowledge pack %s: %w", name, ErrScanning)
	case r.State != StateReady:
		return Entry{}, fmt.Errorf("download knowledge pack %s: it is %q in Agentry, with no tree to download", name, r.State)
	}
	sealed, digest, err := reg.GetTree(ctx, r.ID)
	if err != nil {
		return Entry{}, fmt.Errorf("download knowledge pack %s: %w", name, err)
	}
	h, err := packseal.ReadHeader(bytes.NewReader(sealed))
	if err != nil {
		return Entry{}, fmt.Errorf("download knowledge pack %s: %w", name, err)
	}
	if !validID(h.Pack) || h.Digest != r.Digest || digest != r.Digest {
		return Entry{}, fmt.Errorf("download knowledge pack %s: the tree served is pack %q with digest %q, which does not match the pack Agentry describes (%q)", name, h.Pack, h.Digest, r.Digest)
	}
	key, err := reg.Unwrap(ctx, r.WrappedKey)
	if err != nil {
		return Entry{}, fmt.Errorf("download knowledge pack %s: unwrap its key: %w", name, err)
	}
	m, err := s.checkDownload(name, r, h, sealed, key)
	if err != nil {
		return Entry{}, err
	}
	replacing, err := s.downloadTarget(name, h.Pack, r.Digest)
	if err != nil {
		return Entry{}, err
	}
	kr := KeyRecord{Schema: KeySchema, Wrapper: WrapperRegistry, Registry: reg.URL(), WrappedKey: r.WrappedKey,
		Agentry: &AgentryCopy{Registry: reg.URL(), ID: r.ID, Digest: r.Digest}}
	e, err := s.install(m, kr, func(w io.Writer) error { _, err := w.Write(sealed); return err }, replacing)
	if err != nil {
		return Entry{}, fmt.Errorf("download knowledge pack %s: keep it: %w", name, err)
	}
	_ = s.uncache(h.Pack)
	s.cache(h.Pack, key)
	return e, nil
}

// checkDownload opens a downloaded tree into a private scratch directory,
// holds it to the pack format, and answers the manifest it is kept under:
// the counts are the tree's, the scan record the registry's.
func (s *Store) checkDownload(name string, r Remote, h packseal.Header, sealed, key []byte) (knowledge.Manifest, error) {
	scratch, err := s.stageDir()
	if err != nil {
		return knowledge.Manifest{}, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	_, plain, err := packseal.Open(bytes.NewReader(sealed), key)
	if err != nil {
		return knowledge.Manifest{}, fmt.Errorf("download knowledge pack %s: %w", name, err)
	}
	d, err := knowledge.ExtractTree(plain, scratch, h.Digest)
	if err != nil {
		return knowledge.Manifest{}, fmt.Errorf("download knowledge pack %s: %w", name, err)
	}
	pages, err := knowledge.Pages(scratch)
	if err != nil {
		return knowledge.Manifest{}, err
	}
	m := knowledge.Manifest{
		Schema: knowledge.Schema, Name: r.Name, ID: h.Pack,
		Created: r.Created.UTC().Truncate(time.Second), Updated: r.Updated.UTC().Truncate(time.Second),
		Pages: pages, Bytes: d.Bytes, SHA256: d.SHA256,
	}
	if len(r.Scan) > 0 {
		m.Scan = knowledge.Scan{Scanned: true, Report: r.Scan}
	}
	if err := writeJSON(filepath.Join(scratch, knowledge.ManifestFile), m); err != nil {
		return knowledge.Manifest{}, err
	}
	if _, err := knowledge.Load(scratch); err != nil {
		return knowledge.Manifest{}, fmt.Errorf("download knowledge pack %s: %w", name, err)
	}
	return m, nil
}

// downloadTarget is the entry a download replaces ("" for none): only the
// same pack under the same name, and only when nothing would be lost —
// its tree here is the one downloaded, or the one it last uploaded. A
// different pack of the name, the same pack under another name, or one
// changed here since its upload is refused.
func (s *Store) downloadTarget(name, id, digest string) (string, error) {
	entries, _ := s.List()
	for _, e := range entries {
		switch {
		case e.ID() == id && e.Name() != name:
			return "", fmt.Errorf("download knowledge pack %s: %w (it is the pack kept here as %s; delete or rename that first)", name, ErrNameTaken, e.Name())
		case e.Name() == name && e.ID() != id:
			return "", fmt.Errorf("download knowledge pack %s: %w (another pack, %s; delete or rename it first)", name, ErrNameTaken, e.ID())
		case e.Name() == name:
			unchanged := e.Manifest.SHA256 == digest || (e.Key.Agentry != nil && e.Key.Agentry.Digest == e.Manifest.SHA256)
			if !unchanged {
				return "", fmt.Errorf("download knowledge pack %s: it changed on this computer since Agentry's copy was made; upload it, or delete it here, first", name)
			}
			return e.Dir, nil
		}
	}
	return "", nil
}

package store

import (
	"bufio"
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
	"sync"
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
	// Shared resolves a signed-in client for an organization's shared
	// packs at a registry (nil: this client keeps no shared pack).
	Shared SharedRegistries
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
// clients show (never "safe", "secure" or "private").
func (e Entry) KeyHolder() string {
	if e.Key.Org != "" {
		return e.Key.Org + "'s organization key at " + hostOf(e.Key.Registry) + ", which Agentry holds and hands only to its current members"
	}
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

// shared is the signed-in client of org's shared packs at the registry at u.
func (s *Store) shared(u, org string) (Shared, error) {
	if s.opts.Shared == nil {
		return nil, fmt.Errorf("%s: %w", u, ErrSignedOut)
	}
	return s.opts.Shared(u, org)
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
// made it before sweep removes it. The age is the directory's own
// modification time, which nothing moves while the Creator runs: its pages
// are written in nested directories, and only the apply step at the end,
// which stages beside the pack, touches the directory itself. So a build's
// working copy is kept from the sweep two ways: the process that holds it
// never sweeps it (held), and touches it every scratchTouchEvery so that
// no other process on the same store takes it for a leftover. A run's
// attachment needs neither: it is copied into the run long before.
const staleAfter = time.Hour

// scratchTouchEvery is how often a held working copy is touched. It is
// short beside staleAfter because the ticker does not run while the
// computer sleeps: after a sleep longer than staleAfter the copy reads
// stale to another process until the next touch, and this is how long that
// can be. A variable so a test can watch a touch happen.
var scratchTouchEvery = time.Minute

// held are the working copies this process has been given and has not given
// back, by path. It is the process's, not one Store's: every command, and
// the desktop at every call, makes a Store of its own over the same runner
// home.
var held = struct {
	sync.Mutex
	dirs map[string]struct{}
}{dirs: map[string]struct{}{}}

func isHeld(dir string) bool {
	held.Lock()
	defer held.Unlock()
	_, ok := held.dirs[dir]
	return ok
}

// sweep removes private directories a process that died left behind; one
// this process holds is never among them, however old it reads.
func (s *Store) sweep() {
	dirs, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() || (!strings.HasPrefix(d.Name(), ".stage-") && !strings.HasPrefix(d.Name(), ".open-")) {
			continue
		}
		dir := filepath.Join(s.dir, d.Name())
		if isHeld(dir) {
			continue
		}
		if fi, err := d.Info(); err == nil && time.Since(fi.ModTime()) > staleAfter {
			_ = os.RemoveAll(dir)
		}
	}
}

// Scratch is a fresh 0700 directory inside the store for a pack in the
// clear while something works on it (the Creator making a pack or adding
// to a kept one), and the function that removes it. Until that function is
// called the directory is held: no sweep in this process removes it, and
// it is touched as time passes so that no other process's does; once it
// returns, nothing in this process touches that path again. A build
// may run for an hour or more, and until it applies what the Creator wrote
// its working copy reads as old as the build: a sweep in that time (a
// Download in the desktop beside a build, another command's build or run)
// would take it for a leftover and remove it under the build.
func (s *Store) Scratch() (dir string, done func(), err error) {
	if dir, done, err = s.scratch(); err != nil {
		return "", done, err
	}
	held.Lock()
	held.dirs[dir] = struct{}{}
	held.Unlock()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func(every time.Duration) {
		defer close(stopped)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				_ = os.Chtimes(dir, now, now)
			}
		}
	}(scratchTouchEvery)
	var once sync.Once // closing stop twice would panic
	remove := done
	return dir, func() {
		once.Do(func() {
			// Wait for the toucher to stop before the path is given up: a
			// tick it has already taken would otherwise touch whatever is
			// at that path next.
			close(stop)
			<-stopped
			held.Lock()
			delete(held.dirs, dir)
			held.Unlock()
			remove()
		})
	}, nil
}

// scratch is a fresh 0700 directory inside the store and the function that
// removes it, swept like any other private directory once it is an hour
// old.
func (s *Store) scratch() (dir string, done func(), err error) {
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
	dir, done, err := s.scratch()
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
	if e.Key.Org != "" {
		// An organization's shared pack: its key is asked for every time,
		// never read from the keychain nor written to it, so the registry's
		// check of membership is made at every open.
		sh, err := s.shared(e.Key.Registry, e.Key.Org)
		if err != nil {
			return nil, false, fmt.Errorf("knowledge pack %s: it is %s's shared pack at %s: %w", e.Name(), e.Key.Org, e.Key.Registry, err)
		}
		k, err := sh.Unwrap(ctx, e.Key.WrappedKey)
		if err != nil {
			return nil, false, fmt.Errorf("knowledge pack %s: unwrap %s's key at %s: %w", e.Name(), e.Key.Org, e.Key.Registry, err)
		}
		if len(k) != packseal.KeySize {
			return nil, false, fmt.Errorf("knowledge pack %s: %s answered a key of %d bytes", e.Name(), e.Key.Registry, len(k))
		}
		return k, false, nil
	}
	return s.ownKey(ctx, e.ID(), "knowledge pack "+e.Name(), e.Key, skipCache)
}

// ownKey is the data key of something this store keeps for the person
// (a pack, or the Writing Voice): the keychain's cached copy, else
// unwrapped by the registry that wrapped it (and cached), else by this
// machine's key. what names it in an error.
func (s *Store) ownKey(ctx context.Context, id, what string, kr KeyRecord, skipCache bool) (key []byte, fromCache bool, err error) {
	if !skipCache {
		if k, ok := s.cached(id); ok {
			return k, true, nil
		}
	}
	switch kr.Wrapper {
	case WrapperMachine:
		k, err := s.machineUnwrap(id, kr.WrappedKey)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", what, err)
		}
		return k, false, nil
	case WrapperRegistry:
		reg, err := s.registry(kr.Registry)
		if err != nil {
			return nil, false, fmt.Errorf("%s: its key is wrapped by %s: %w", what, kr.Registry, err)
		}
		k, err := reg.Unwrap(ctx, kr.WrappedKey)
		if err != nil {
			return nil, false, fmt.Errorf("%s: unwrap its key at %s: %w", what, kr.Registry, err)
		}
		if len(k) != packseal.KeySize {
			return nil, false, fmt.Errorf("%s: %s answered a key of %d bytes", what, kr.Registry, len(k))
		}
		s.cache(id, k)
		return k, false, nil
	}
	return nil, false, fmt.Errorf("%s: its key is wrapped by %q, which this client does not know", what, kr.Wrapper)
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
	// The sealed tree is streamed, never read whole: a pack kept here may
	// be up to knowledge.MaxLocalBytes.
	sealed, err := os.Open(filepath.Join(e.Dir, SealedFile)) //nolint:gosec // an entry of the store
	if err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", e.Name(), err)
	}
	defer func() { _ = sealed.Close() }()
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
		if _, err := sealed.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("knowledge pack %s: %w", e.Name(), err)
		}
		p, err = s.openWith(e, sealed, key, dst)
	}
	return p, err
}

func (s *Store) openWith(e Entry, sealed io.Reader, key []byte, dst string) (*knowledge.Pack, error) {
	h, plain, err := packseal.Open(bufio.NewReaderSize(sealed, 1<<20), key)
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
	if e.Key.Org != "" {
		return Entry{}, fmt.Errorf("knowledge pack %s: %w", name, ErrShared)
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
	if everywhere && e.Key.Org != "" {
		return Entry{}, fmt.Errorf("knowledge pack %s: delete Agentry's copy: %w; delete it from this computer alone", name, ErrShared)
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
// registry's wrapper (at the next sign-in), and answers their names.
// Signed out, it does nothing.
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
	if e.Key.Org != "" {
		return Entry{}, Remote{}, fmt.Errorf("upload knowledge pack %s: %w", name, ErrShared)
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
	// Held to Agentry's cap by the manifest's count and by the sealed
	// file's own size, so an edited manifest cannot make this read a
	// gigabyte for the registry to refuse.
	sealedSize := int64(-1)
	if fi, err := os.Stat(filepath.Join(e.Dir, SealedFile)); err == nil {
		sealedSize = fi.Size()
	}
	if e.Manifest.Bytes > knowledge.MaxBytes || sealedSize > packseal.MaxSealedSize(knowledge.MaxBytes) {
		return Entry{}, Remote{}, fmt.Errorf("upload knowledge pack %s: it is %d bytes and Agentry keeps packs of at most %s, so it stays on this computer: %w",
			name, e.Manifest.Bytes, knowledge.FormatCap(knowledge.MaxBytes), ErrPastAgentryCap)
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
	return s.download(ctx, reg, "", name)
}

// source is what a download reads a pack from: the person's own packs at a
// registry (Registry), or an organization's shared ones (Shared).
type source interface {
	URL() string
	List(ctx context.Context) ([]Remote, error)
	GetTree(ctx context.Context, id string) (sealed []byte, digest string, err error)
	Unwrap(ctx context.Context, wk WrappedKey) ([]byte, error)
}

// download is the one path a pack is fetched and kept by: listed by name
// at src, its sealed tree checked against what src describes, opened with
// the key src unwraps and checked in full, kept under that wrapped key. A
// person's own pack's key is cached; an organization's (org set) is not,
// and a key a personal pack of the same id left in the keychain is removed.
func (s *Store) download(ctx context.Context, src source, org, name string) (Entry, error) {
	what := "knowledge pack " + name
	if org != "" {
		what = org + "'s knowledge pack " + name
	}
	remotes, err := src.List(ctx)
	if err != nil {
		return Entry{}, fmt.Errorf("download %s: %w", what, err)
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
		return Entry{}, fmt.Errorf("download %s: %w", what, ErrRemoteNotFound)
	case r.State == StateScanning:
		return Entry{}, fmt.Errorf("download %s: %w", what, ErrScanning)
	case r.State != StateReady:
		return Entry{}, fmt.Errorf("download %s: it is %q in Agentry, with no tree to download", what, r.State)
	}
	sealed, digest, err := src.GetTree(ctx, r.ID)
	if err != nil {
		return Entry{}, fmt.Errorf("download %s: %w", what, err)
	}
	h, err := packseal.ReadHeader(bytes.NewReader(sealed))
	if err != nil {
		return Entry{}, fmt.Errorf("download %s: %w", what, err)
	}
	if !validID(h.Pack) || h.Digest != r.Digest || digest != r.Digest {
		return Entry{}, fmt.Errorf("download %s: the tree served is pack %q with digest %q, which does not match the pack Agentry describes (%q)", what, h.Pack, h.Digest, r.Digest)
	}
	key, err := src.Unwrap(ctx, r.WrappedKey)
	if err != nil {
		return Entry{}, fmt.Errorf("download %s: unwrap its key: %w", what, err)
	}
	m, err := s.checkDownload(name, r, h, sealed, key)
	if err != nil {
		return Entry{}, err
	}
	replacing, err := s.downloadTarget(name, h.Pack, r.Digest)
	if err != nil {
		return Entry{}, err
	}
	kr := KeyRecord{Schema: KeySchema, Wrapper: WrapperRegistry, Registry: src.URL(), Org: org, WrappedKey: r.WrappedKey,
		Agentry: &AgentryCopy{Registry: src.URL(), ID: r.ID, Digest: r.Digest}}
	e, err := s.install(m, kr, func(w io.Writer) error { _, err := w.Write(sealed); return err }, replacing)
	if err != nil {
		return Entry{}, fmt.Errorf("download %s: keep it: %w", what, err)
	}
	if err := s.uncache(h.Pack); err != nil && org != "" {
		return e, fmt.Errorf("%s was kept, but a key a pack of its id left in the OS keychain would not leave it: %w", what, err)
	}
	if org == "" {
		s.cache(h.Pack, key)
	}
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
	m := knowledge.Manifest{
		Name: r.Name, ID: h.Pack,
		Created: r.Created.UTC().Truncate(time.Second), Updated: r.Updated.UTC().Truncate(time.Second),
	}
	if len(r.Scan) > 0 {
		m.Scan = knowledge.Scan{Scanned: true, Report: r.Scan}
	}
	p, err := knowledge.Unpack(plain, h.Digest, scratch, m)
	if err != nil {
		return knowledge.Manifest{}, fmt.Errorf("download knowledge pack %s: %w", name, err)
	}
	return p.Manifest, nil
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

// DownloadShared keeps the pack named name that org shares at the current
// registry: its sealed tree, opened with the key the organization's route
// unwraps and checked in full, then kept here under that wrapped key and
// the organization's name. Nothing of the key is cached; every later open
// asks the registry again. A pack kept here with the same id is replaced,
// as a download replaces one; another pack of that name, the person's own
// included, is not.
func (s *Store) DownloadShared(ctx context.Context, org, name string) (Entry, error) {
	s.sweep()
	if s.opts.Current == "" {
		return Entry{}, fmt.Errorf("download %s's knowledge pack %s: %w", org, name, ErrSignedOut)
	}
	sh, err := s.shared(s.opts.Current, org)
	if err != nil {
		return Entry{}, fmt.Errorf("download %s's knowledge pack %s: %w", org, name, err)
	}
	return s.download(ctx, sh, org, name)
}

// Export writes the pack named name, in the clear, into dst (new, or an
// empty directory), and returns it loaded and verified. The person's own
// pack is opened here. An organization's shared pack is exported by the
// registry, which decides whether this person may (its admins always, a
// member only once the pack is marked exportable; ErrNotExportable
// otherwise), and its tree is the one Agentry keeps now.
func (s *Store) Export(ctx context.Context, name, dst string) (*knowledge.Pack, error) {
	e, err := s.Find(name)
	if err != nil {
		return nil, err
	}
	if e.Key.Org == "" {
		return s.openEntry(ctx, e, dst)
	}
	if e.Key.Agentry == nil {
		return nil, fmt.Errorf("export knowledge pack %s: its record names no copy in Agentry to export", name)
	}
	sh, err := s.shared(e.Key.Registry, e.Key.Org)
	if err != nil {
		return nil, fmt.Errorf("export knowledge pack %s: %w", name, err)
	}
	tree, digest, err := sh.Export(ctx, e.Key.Agentry.ID)
	if err != nil {
		return nil, fmt.Errorf("export knowledge pack %s: %w", name, err)
	}
	if digest == "" {
		return nil, fmt.Errorf("export knowledge pack %s: the registry named no digest for the tree it sent, so it cannot be checked", name)
	}
	m := e.Manifest
	p, err := knowledge.Unpack(bytes.NewReader(tree), digest, dst, m)
	if err != nil {
		return nil, fmt.Errorf("export knowledge pack %s: %w", name, err)
	}
	return p, nil
}

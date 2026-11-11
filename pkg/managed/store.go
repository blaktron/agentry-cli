package managed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/configdir"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// cacheFile is the cache's format on disk: the envelope exactly as the
// registry served it, and the registry it came from. Nothing in it is
// trusted until the envelope verifies again.
type cacheFile struct {
	Format    int                      `json:"format"`
	Registry  string                   `json:"registry"`
	FetchedAt time.Time                `json:"fetchedAt"`
	Envelope  managedsettings.Envelope `json:"envelope"`
}

// What a registry last answered, as seenFile.Holds records it.
const (
	holdsDocument  = "document"
	holdsNone      = "none"
	holdsSignedOut = "signed-out"
)

// seenFile is this client's record of a registry's answers, beside the
// cache: the account they were for, what the registry last answered and
// when, and the newest version accepted from each organization. A cached
// document older than its organization's newest is a replayed copy, and a
// record that says a document was accepted, with none beside it, is a
// document that was removed; both are refused.
type seenFile struct {
	Format   int    `json:"format"`
	Registry string `json:"registry"`
	// Account is the id of the account the answers were for.
	Account string `json:"account,omitempty"`
	// Holds is what the registry last answered: a document, that none
	// apply, or nothing since the account signed out.
	Holds      string    `json:"holds,omitempty"`
	AnsweredAt time.Time `json:"answeredAt,omitzero"`
	// Versions is the newest version accepted from each organization.
	Versions map[string]int64 `json:"versions,omitempty"`
}

const (
	cacheFormat = 1
	seenFormat  = 1
)

// futureSlack is how far ahead of this computer's clock a time it wrote
// itself may be: a time past it was not written by this client.
const futureSlack = time.Minute

func normalize(registryURL string) string { return trust.NormalizeRegistry(registryURL) }

// Dir is the folder the cached documents live in.
func Dir() (string, error) {
	dir, err := configdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "managed"), nil
}

// paths are the registry's cached document and its record of answers.
func paths(registryURL string) (doc, seen string, err error) {
	dir, err := Dir()
	if err != nil {
		return "", "", err
	}
	stem := filepath.Join(dir, trust.RegistryFileStem(registryURL))
	return stem + ".json", stem + ".seen", nil
}

// CachePath is where registryURL's verified document is kept.
func CachePath(registryURL string) (string, error) {
	doc, _, err := paths(registryURL)
	return doc, err
}

// Load reads registryURL's cached document and verifies it again: the
// signature with a key pinned for the registry, the document's binding to
// the registry, that it is not older than a version already applied, and
// that a document the registry last answered is still there. It returns
// nil with no error when none apply, and an error when what is held cannot
// be verified.
func Load(registryURL string) (*State, error) {
	st, p := inspect(normalize(registryURL))
	if p != nil {
		return nil, p.Err
	}
	return st, nil
}

// Current loads every registry's managed settings and verifies each again.
// It never fails: what cannot be read or verified is a Problem.
func Current() Effective {
	e := current()
	m, err := LoadMachine()
	switch {
	case err != nil:
		e.Problems = append([]Problem{{Err: err, Machine: true}}, e.Problems...)
	case m != nil:
		e.Machine = m
		if len(m.Settings) > 0 {
			e.Documents = append([]State{m.machineDocument()}, e.Documents...)
		}
	}
	return e
}

// current is every registry's verified documents and problems, without
// the machine settings.
func current() Effective {
	var e Effective
	dir, err := Dir()
	if err != nil {
		e.Problems = append(e.Problems, Problem{Err: err})
		return e
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return e
	}
	if err != nil {
		e.Problems = append(e.Problems, Problem{Err: fmt.Errorf("read %s: %w", dir, err)})
		return e
	}
	regs := map[string]bool{}
	for _, ent := range entries {
		name := ent.Name()
		ext := filepath.Ext(name)
		if ent.IsDir() || strings.HasPrefix(name, ".") || (ext != ".json" && ext != ".seen") {
			continue
		}
		reg, err := namedRegistry(filepath.Join(dir, name))
		if err == nil && trust.RegistryFileStem(reg)+ext != name {
			err = fmt.Errorf("%s holds the managed settings of %s, which are kept under another name", name, reg)
		}
		if err != nil {
			e.Problems = append(e.Problems, Problem{Registry: name, Err: err})
			continue
		}
		regs[normalize(reg)] = true
	}
	for _, reg := range sortedKeys(regs) {
		st, p := inspect(reg)
		switch {
		case p != nil:
			e.Problems = append(e.Problems, *p)
		case st != nil:
			e.Documents = append(e.Documents, *st)
		}
	}
	return e
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// namedRegistry reads the registry a cache file or a record says it is
// for; inspect then holds the file to it.
func namedRegistry(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a file under the config directory's managed folder
	if err != nil {
		return "", err
	}
	var f struct {
		Registry string `json:"registry"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.Registry == "" {
		return "", fmt.Errorf("%s is not a managed settings file", filepath.Base(path))
	}
	return f.Registry, nil
}

// inspect is one registry's managed settings: the verified document, none,
// or the problem with what is held.
func inspect(reg string) (*State, *Problem) {
	fail := func(err error) (*State, *Problem) {
		return nil, &Problem{Registry: reg, Err: err}
	}
	seen, err := readSeen(reg)
	if err != nil {
		return fail(err)
	}
	now := time.Now()
	if seen.AnsweredAt.After(now.Add(futureSlack)) {
		return fail(fmt.Errorf("the record of the registry's answers is dated %s, ahead of this computer's clock", seen.AnsweredAt.UTC().Format(time.RFC3339)))
	}
	docPath, _, err := paths(reg)
	if err != nil {
		return fail(err)
	}
	raw, err := os.ReadFile(docPath) //nolint:gosec // a fixed name under the config directory
	if errors.Is(err, os.ErrNotExist) {
		if seen.Holds == holdsDocument {
			return fail(errors.New("the document accepted from the registry is no longer on this computer"))
		}
		return nil, nil
	}
	if err != nil {
		return fail(fmt.Errorf("read the cached document: %w", err))
	}
	var f cacheFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return fail(fmt.Errorf("the cached document: %w", err))
	}
	switch {
	case f.Format != cacheFormat:
		return fail(fmt.Errorf("the cached document has format %d, want %d", f.Format, cacheFormat))
	case normalize(f.Registry) != reg:
		return fail(fmt.Errorf("the cached document names another registry, %s", f.Registry))
	case f.FetchedAt.After(now.Add(futureSlack)):
		return fail(fmt.Errorf("the cached document is dated %s, ahead of this computer's clock", f.FetchedAt.UTC().Format(time.RFC3339)))
	}
	doc, err := open(reg, f.Envelope)
	if err != nil {
		return fail(err)
	}
	if err := seen.notOlder(doc); err != nil {
		return fail(err)
	}
	// A document bound to its members is held to the account the answers
	// were for, so one signed for another member is refused here too.
	if err := doc.For(seen.Account); err != nil {
		return fail(err)
	}
	return &State{Registry: reg, Document: doc, FetchedAt: f.FetchedAt}, nil
}

// open verifies env with each key pinned for the registry that the
// registry has not retired, never choosing a key by the signature's key
// id, and holds the document to the registry. The pins are the registry's
// own, whatever AGENTRY_PIN_REGISTRY names: a document is about the
// registry that signed it.
func open(reg string, env managedsettings.Envelope) (managedsettings.Document, error) {
	set, err := trust.LoadOwnPinned(reg)
	if err != nil {
		return managedsettings.Document{}, fmt.Errorf("read the signing keys pinned for %s: %w", reg, err)
	}
	var tried int
	var last error
	for _, k := range set.Keys {
		if k.Retired() {
			continue
		}
		tried++
		doc, err := managedsettings.Open(env, k.PublicKey)
		if err != nil {
			last = err
			continue
		}
		if normalize(doc.Registry) != reg {
			return managedsettings.Document{}, fmt.Errorf("the document was issued by %s, not %s", doc.Registry, reg)
		}
		return doc, nil
	}
	if tried == 0 {
		return managedsettings.Document{}, fmt.Errorf("no signing key is pinned for %s: sign in to it again", reg)
	}
	return managedsettings.Document{}, fmt.Errorf("no signing key pinned for %s verifies the document: %w", reg, last)
}

// notOlder refuses a document older than the newest one accepted from its
// organization. A document carries no expiry, so without this a copy kept
// from before a stricter version could be put back.
func (s seenFile) notOlder(doc managedsettings.Document) error {
	if newest := s.Versions[doc.Org]; doc.Version < newest {
		return fmt.Errorf("version %d of %s's managed settings is older than version %d, which this computer already applied", doc.Version, doc.Org, newest)
	}
	return nil
}

// readSeen reads the registry's record of answers; an empty one when there
// is none.
func readSeen(reg string) (seenFile, error) {
	_, path, err := paths(reg)
	if err != nil {
		return seenFile{}, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // a fixed name under the config directory
	if errors.Is(err, os.ErrNotExist) {
		return seenFile{Format: seenFormat, Registry: reg}, nil
	}
	if err != nil {
		return seenFile{}, fmt.Errorf("read the record of the registry's answers: %w", err)
	}
	var s seenFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return seenFile{}, fmt.Errorf("the record of the registry's answers: %w", err)
	}
	if s.Format != seenFormat || normalize(s.Registry) != reg {
		return seenFile{}, errors.New("the record of the registry's answers is not this registry's")
	}
	return s, nil
}

func writeSeen(reg string, s seenFile) error {
	_, path, err := paths(reg)
	if err != nil {
		return err
	}
	s.Format, s.Registry = seenFormat, reg
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(path, append(raw, '\n')); err != nil {
		return fmt.Errorf("record the managed settings answer from %s: %w", reg, err)
	}
	return nil
}

// fresh says the registry answered within maxAge, by this client's own
// record, and what it answered still verifies.
func fresh(reg string, maxAge time.Duration) bool {
	seen, err := readSeen(reg)
	if err != nil || seen.Holds == "" || seen.Holds == holdsSignedOut {
		return false
	}
	age := time.Since(seen.AnsweredAt)
	return age >= -futureSlack && age < maxAge
}

// SignedIn records that accountID is now signed in to the registry. When
// another account was, its managed settings are deleted: they were never
// this account's. The same account's are kept, so a sign-in the registry
// cannot answer at that moment does not lift its organization's locks.
func SignedIn(registryURL, accountID string) error {
	reg := normalize(registryURL)
	seen, err := readSeen(reg)
	if err != nil {
		// A record that cannot be read is replaced; the next answer
		// decides what applies.
		seen = seenFile{}
	}
	if seen.Account == accountID && err == nil {
		return nil
	}
	if seen.Account != "" && seen.Account != accountID {
		if err := removeDocument(reg); err != nil {
			return err
		}
		seen.Holds, seen.AnsweredAt = "", time.Time{}
	}
	seen.Account = accountID
	return writeSeen(reg, seen)
}

// Forget deletes the registry's cached document at sign-out. The newest
// version accepted from each organization is kept, so an older copy cannot
// be put back later.
func Forget(registryURL string) error {
	reg := normalize(registryURL)
	if err := removeDocument(reg); err != nil {
		return err
	}
	seen, err := readSeen(reg)
	if err != nil {
		seen = seenFile{}
	}
	seen.Account, seen.Holds, seen.AnsweredAt = "", holdsSignedOut, time.Now().UTC()
	return writeSeen(reg, seen)
}

func removeDocument(reg string) error {
	doc, _, err := paths(reg)
	if err != nil {
		return err
	}
	// The keys an organization delivered go with its document.
	if err := removeKeys(reg); err != nil {
		return err
	}
	if err := os.Remove(doc); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete the managed settings cached for %s: %w", reg, err)
	}
	return nil
}

// writeFile replaces path whole with data, 0600, in a folder only the
// user can enter, and makes it durable before it takes the name.
func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".managed-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Package managed is a client's copy of the managed settings an
// organization's admins set for its members: the registry's signed
// document (github.com/blaktron/agentry/pkg/managedsettings), fetched with
// the account's own token, verified against the keys pinned for that
// registry at sign-in, held to that registry and to the organization the
// account is an active member of, and kept in the config directory so
// that it still applies offline.
//
// Every load verifies the cached copy again: the cache is the signed
// envelope as the registry served it, never a decoded copy, so an edit to
// it is refused. A document only ever narrows what a member could choose,
// so the engine applies every verified document it holds, whichever
// registry a run comes from.
package managed

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/internal/origin"
	"github.com/blaktron/agentry-cli/pkg/configdir"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// Route is the registry's answer to the signed-in caller.
const Route = "/api/v1/user/managed-settings"

// FetchInterval is how old the last answer may be before a run asks the
// registry again.
const FetchInterval = time.Hour

// maxEnvelopeBytes bounds what a fetch reads: a document is a few
// kilobytes.
const maxEnvelopeBytes = 1 << 20

// Enforced are the settings this client applies. A document's other
// known rows are shown but not applied, and rows the catalogue does not
// know are judged by Document.Unknown.
var Enforced = []string{managedsettings.UnsafeMode}

// State is one registry's verified managed settings.
type State struct {
	// Registry is the registry's normalised address.
	Registry string
	// Document is the verified document, bound to Registry.
	Document managedsettings.Document
	// FetchedAt is when this client last read it from the registry.
	FetchedAt time.Time
}

// Locked is the value of a setting the organization locked, and whether
// it is locked.
func (s *State) Locked(id string) (string, bool) {
	if s == nil {
		return "", false
	}
	v, ok := s.Document.Settings[id]
	if !ok || !v.Locked {
		return "", false
	}
	return v.String(), true
}

// Applied are the ids of the document's settings this client applies,
// sorted.
func (s *State) Applied() []string {
	var out []string
	for _, id := range Enforced {
		if _, ok := s.Document.Settings[id]; ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// cacheFile is the cache's format on disk: the envelope exactly as the
// registry served it, and the registry it came from. Nothing in it is
// trusted until the envelope verifies again.
type cacheFile struct {
	Format    int                      `json:"format"`
	Registry  string                   `json:"registry"`
	FetchedAt time.Time                `json:"fetchedAt"`
	Envelope  managedsettings.Envelope `json:"envelope"`
}

// seenFile is the newest document this client accepted from a registry: a
// document from the same organization with a lower version is an older
// copy being replayed, and is refused. Another organization's document,
// after the account's membership changed, starts the count again.
type seenFile struct {
	Org     string `json:"org"`
	Version int64  `json:"version"`
}

const cacheFormat = 1

// Dir is the folder the cached documents live in.
func Dir() (string, error) {
	dir, err := configdir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "managed"), nil
}

// paths are the registry's cached document, the stamp of a registry that
// answered that none apply, and the newest version accepted from it.
func paths(registryURL string) (doc, none, seen string, err error) {
	dir, err := Dir()
	if err != nil {
		return "", "", "", err
	}
	stem := filepath.Join(dir, trust.RegistryFileStem(registryURL))
	return stem + ".json", stem + ".none", stem + ".seen", nil
}

// CachePath is where registryURL's verified document is kept.
func CachePath(registryURL string) (string, error) {
	doc, _, _, err := paths(registryURL)
	return doc, err
}

// Load reads registryURL's cached document and verifies it again: the
// signature with a key pinned for the registry, the document's binding to
// the registry, and that it is not older than the newest one accepted. It
// returns nil with no error when nothing is cached, and an error when a
// cached copy does not verify.
func Load(registryURL string) (*State, error) {
	path, _, _, err := paths(registryURL)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // a fixed name under the config directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the managed settings cached for %s: %w", registryURL, err)
	}
	var f cacheFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("the managed settings cached for %s: %w", registryURL, err)
	}
	if f.Format != cacheFormat {
		return nil, fmt.Errorf("the managed settings cached for %s have format %d, want %d", registryURL, f.Format, cacheFormat)
	}
	reg := trust.NormalizeRegistry(registryURL)
	if trust.NormalizeRegistry(f.Registry) != reg {
		return nil, fmt.Errorf("the managed settings cached for %s name another registry, %s", registryURL, f.Registry)
	}
	doc, err := open(reg, f.Envelope)
	if err != nil {
		return nil, fmt.Errorf("the managed settings cached for %s: %w", registryURL, err)
	}
	if err := notOlder(registryURL, doc); err != nil {
		return nil, fmt.Errorf("the managed settings cached for %s: %w", registryURL, err)
	}
	return &State{Registry: reg, Document: doc, FetchedAt: f.FetchedAt}, nil
}

// open verifies env with each key pinned for the registry that the
// registry has not retired, never choosing a key by the signature's key
// id, and holds the document to the registry.
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
		if trust.NormalizeRegistry(doc.Registry) != reg {
			return managedsettings.Document{}, fmt.Errorf("the document was issued by %s, not %s", doc.Registry, reg)
		}
		return doc, nil
	}
	if tried == 0 {
		return managedsettings.Document{}, fmt.Errorf("no signing key is pinned for %s: sign in to it again", reg)
	}
	return managedsettings.Document{}, fmt.Errorf("no signing key pinned for %s verifies the document: %w", reg, last)
}

// notOlder refuses a document older than the newest one this client
// accepted from the registry for the same organization. A
// document carries no expiry, so without this a copy kept from before a
// stricter version could be put back in the cache.
func notOlder(registryURL string, doc managedsettings.Document) error {
	_, _, seenPath, err := paths(registryURL)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(seenPath) //nolint:gosec // a fixed name under the config directory
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var seen seenFile
	if err := json.Unmarshal(raw, &seen); err != nil {
		return fmt.Errorf("read the newest version accepted: %w", err)
	}
	if seen.Org == doc.Org && doc.Version < seen.Version {
		return fmt.Errorf("version %d of %s's managed settings is older than version %d, which this computer already applied", doc.Version, doc.Org, seen.Version)
	}
	return nil
}

// Refresh asks the registry for the signed-in account's managed settings
// with the account's token, and keeps what it answers: a document that
// verifies, is bound to this registry, is the settings of the organization
// the account is an active member of, and is not older than the last one
// accepted replaces the cache; 204 (none apply) or 404
// (no longer a member) deletes it. It returns nil when none apply. On any
// other answer, or a document that is refused, the cache is left as it
// was and the error says why.
func Refresh(ctx context.Context, hc *http.Client, registryURL, token string) (*State, error) {
	if token == "" {
		return nil, fmt.Errorf("not signed in to %s", registryURL)
	}
	reg := trust.NormalizeRegistry(registryURL)
	hc = origin.Guard(hc)
	org, err := activeOrg(ctx, hc, reg, token)
	if err != nil {
		return nil, err
	}
	res, err := get(ctx, hc, reg+Route, token)
	if err != nil {
		return nil, fmt.Errorf("fetch managed settings from %s: %w", reg, err)
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil, cleared(reg)
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("fetch managed settings from %s: HTTP %d", reg, res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxEnvelopeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch managed settings from %s: %w", reg, err)
	}
	if len(body) > maxEnvelopeBytes {
		return nil, fmt.Errorf("fetch managed settings from %s: the answer is larger than %d bytes", reg, maxEnvelopeBytes)
	}
	var env managedsettings.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("managed settings from %s: decode the envelope: %w", reg, err)
	}
	doc, err := open(reg, env)
	if err != nil {
		return nil, fmt.Errorf("managed settings from %s: %w", reg, err)
	}
	switch {
	case org == "":
		return nil, fmt.Errorf("managed settings from %s: the document is %s's, and the signed-in account is an active member of no organization", reg, doc.Org)
	case doc.Org != org:
		return nil, fmt.Errorf("managed settings from %s: the document is %s's, not those of %s, the organization the signed-in account is a member of", reg, doc.Org, org)
	}
	if err := notOlder(reg, doc); err != nil {
		return nil, fmt.Errorf("managed settings from %s: %w", reg, err)
	}
	now := time.Now().UTC()
	if err := save(reg, cacheFile{Format: cacheFormat, Registry: reg, FetchedAt: now, Envelope: env}, doc); err != nil {
		return nil, err
	}
	return &State{Registry: reg, Document: doc, FetchedAt: now}, nil
}

// Sync is the refresh a run makes: when the registry last answered within
// maxAge and the cached copy verifies, the cached copy (or none) stands;
// otherwise it asks the registry. When the registry cannot be asked, the
// last verified copy applies and is returned beside the error.
func Sync(ctx context.Context, hc *http.Client, registryURL, token string, maxAge time.Duration) (*State, error) {
	cached, loadErr := Load(registryURL)
	if loadErr == nil && fresh(registryURL, maxAge) {
		return cached, nil
	}
	st, err := Refresh(ctx, hc, registryURL, token)
	if err != nil {
		if loadErr != nil {
			return nil, errors.Join(err, loadErr)
		}
		return cached, err
	}
	return st, nil
}

// fresh says the registry answered within maxAge: the cached document's
// file or the stamp of an answer that none apply was written since.
func fresh(registryURL string, maxAge time.Duration) bool {
	doc, none, _, err := paths(registryURL)
	if err != nil {
		return false
	}
	for _, p := range []string{doc, none} {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) < maxAge {
			return true
		}
	}
	return false
}

// Forget deletes the registry's cached document: at sign-out, and when the
// account changes. The newest version accepted is kept, so an older copy
// of the same organization's settings cannot be put back later.
func Forget(registryURL string) error {
	doc, none, _, err := paths(registryURL)
	if err != nil {
		return err
	}
	for _, p := range []string{doc, none} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete the managed settings cached for %s: %w", registryURL, err)
		}
	}
	return nil
}

// cleared deletes the cached document after the registry answered that
// none apply, and stamps the answer's time.
func cleared(reg string) error {
	if err := Forget(reg); err != nil {
		return err
	}
	_, none, _, err := paths(reg)
	if err != nil {
		return err
	}
	return writeFile(none, nil)
}

// save writes the cache and the newest version accepted, each 0600.
func save(reg string, f cacheFile, doc managedsettings.Document) error {
	docPath, none, seenPath, err := paths(reg)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(docPath, append(raw, '\n')); err != nil {
		return fmt.Errorf("cache the managed settings from %s: %w", reg, err)
	}
	seen, err := json.Marshal(seenFile{Org: doc.Org, Version: doc.Version})
	if err != nil {
		return err
	}
	if err := writeFile(seenPath, seen); err != nil {
		return fmt.Errorf("record the managed settings version from %s: %w", reg, err)
	}
	_ = os.Remove(none)
	return nil
}

// writeFile replaces path whole with data, 0600, in a folder only the
// user can enter.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".managed-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
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

func get(ctx context.Context, hc *http.Client, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return hc.Do(req)
}

// activeOrg is the handle of the organization the token's account is an
// active member of, "" when it is a member of none. A person belongs to
// one organization at most; a membership an admin has yet to approve is
// not active. A registry that does not state a membership's state answers
// only memberships in force.
func activeOrg(ctx context.Context, hc *http.Client, reg, token string) (string, error) {
	res, err := get(ctx, hc, reg+"/api/v1/user", token)
	if err != nil {
		return "", fmt.Errorf("read the signed-in account from %s: %w", reg, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read the signed-in account from %s: HTTP %d", reg, res.StatusCode)
	}
	var u struct {
		Orgs []struct {
			Handle string `json:"handle"`
			State  string `json:"state"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&u); err != nil {
		return "", fmt.Errorf("read the signed-in account from %s: %w", reg, err)
	}
	for _, o := range u.Orgs {
		if o.State == "" || o.State == "active" {
			return o.Handle, nil
		}
	}
	return "", nil
}

// Problem is a cached document that does not verify.
type Problem struct {
	Registry string
	Err      error
}

// Effective is every cached document together, as the engine applies
// them.
type Effective struct {
	// Documents are the verified documents, by registry.
	Documents []State
	// Problems are the cached copies that did not verify.
	Problems []Problem
}

// Current loads every cached document and verifies each again. It never
// fails: a copy that cannot be read or verified is a Problem.
func Current() Effective {
	var e Effective
	dir, err := Dir()
	if err != nil {
		e.Problems = append(e.Problems, Problem{Registry: "", Err: err})
		return e
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return e
	}
	if err != nil {
		e.Problems = append(e.Problems, Problem{Registry: "", Err: fmt.Errorf("read %s: %w", dir, err)})
		return e
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		reg, err := cachedRegistry(filepath.Join(dir, name))
		if err == nil && trust.RegistryFileStem(reg)+".json" != name {
			err = fmt.Errorf("%s holds the managed settings of %s, which are kept under another name", name, reg)
		}
		if err != nil {
			e.Problems = append(e.Problems, Problem{Registry: name, Err: err})
			continue
		}
		st, err := Load(reg)
		switch {
		case err != nil:
			e.Problems = append(e.Problems, Problem{Registry: trust.NormalizeRegistry(reg), Err: err})
		case st != nil:
			e.Documents = append(e.Documents, *st)
		}
	}
	sort.Slice(e.Documents, func(i, j int) bool { return e.Documents[i].Registry < e.Documents[j].Registry })
	return e
}

// cachedRegistry reads the registry a cache file says it came from; Load
// then holds the file to it.
func cachedRegistry(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a file under the config directory's managed folder
	if err != nil {
		return "", err
	}
	var f struct {
		Registry string `json:"registry"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.Registry == "" {
		return "", fmt.Errorf("%s is not a cached managed settings document", filepath.Base(path))
	}
	return f.Registry, nil
}

// RunRefusal is why no run may start at all, or "": a document locks a
// setting this client does not know, so it cannot hold the member to it.
func (e Effective) RunRefusal() string {
	for _, d := range e.Documents {
		for _, u := range d.Document.Unknown() {
			if u.Locked {
				return fmt.Sprintf("%s's managed settings lock %s, which this version of Agentry does not know; update Agentry to run", d.Document.Org, u.ID)
			}
		}
	}
	return ""
}

// UnsafeModeRefusal is why Unsafe Mode may not be turned on, or "" when it
// may: a document that locks run.unsafeMode to forbidden, or a cached copy
// that does not verify, which might.
func (e Effective) UnsafeModeRefusal() string {
	for _, d := range e.Documents {
		if v, ok := d.Locked(managedsettings.UnsafeMode); ok && v == managedsettings.UnsafeModeForbidden {
			return fmt.Sprintf("Unsafe Mode is forbidden by %s's managed settings (%s)", d.Document.Org, managedsettings.UnsafeMode)
		}
	}
	for _, p := range e.Problems {
		return fmt.Sprintf("Unsafe Mode is refused while the managed settings cached for %s do not verify (%v); `agentry managed refresh` reads them again", cmp.Or(p.Registry, "this computer"), p.Err)
	}
	return ""
}

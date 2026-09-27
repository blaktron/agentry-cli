package knowledgetest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/blaktron/agentry/pkg/apiclient"
	"github.com/blaktron/agentry/pkg/packseal"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
)

// Registry is an in-process fake of a core registry's knowledge routes
// (docs/knowledge.md in the core), for the store's and the clients' tests:
// wrap and unwrap for the account a bearer token names and no other,
// create, upload (opened and checked as the core checks it), list,
// download and delete, answered in the core's JSON and problem shapes.
//
// It also serves an organization's shared packs (Knowledge M8; the core's
// /orgs/{org}/knowledge/): list, read, download, unwrap and export, for
// the organization's members alone (404 to anyone else, as the core
// answers), a member's export of a pack not marked exportable 403
// knowledge-not-exportable. Shared packs are seeded with SeedShared; the
// organization's key is the fake's own, bound to the organization.
type Registry struct {
	*httptest.Server

	mu       sync.Mutex
	key      []byte
	accounts map[string]string // token → owner
	packs    map[string]map[string]*fakePack
	orgs     map[string]*fakeOrg
	// Scanning leaves an uploaded pack scanning, as the core does until
	// its job ends.
	Scanning bool
	// Forbid answers 403 for every knowledge route, as the core answers a
	// token minted before the knowledge scopes existed.
	Forbid bool
	// Requests is every request's method and path, in order.
	Requests []string
}

type fakePack struct {
	api    apiclient.KnowledgePack
	sealed []byte
	tree   []byte // a shared pack's canonical tar, for its export
}

// fakeOrg is an organization: its members by role, and its shared packs.
type fakeOrg struct {
	members map[string]string // owner → "admin" | "member"
	packs   map[string]*fakePack
}

// scanReport is the report the fake gives every uploaded pack.
const scanReport = `{"schema":"knowledge-scan/v1","stages":[],"read":{"sources":1,"wiki":1,"bytes":10,"notRead":[]},"review":{"configured":false,"provider":"none","sourceChunks":0,"wikiChunks":0},"findings":[]}`

// NewRegistry starts the fake; it closes with the test.
func NewRegistry(t testing.TB) *Registry {
	t.Helper()
	k, err := packseal.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &Registry{key: k, accounts: map[string]string{}, packs: map[string]map[string]*fakePack{}, orgs: map[string]*fakeOrg{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Close)
	return r
}

// Token is a bearer token for owner, made the first time it is asked for.
func (r *Registry) Token(owner string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok := "agyp_fake_" + owner
	r.accounts[tok] = owner
	return tok
}

// Packs is how many packs owner keeps.
func (r *Registry) Packs(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.packs[owner])
}

// Finish ends every scan, as the core's job does.
func (r *Registry) Finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ps := range r.packs {
		for _, p := range ps {
			if p.api.State == "scanning" {
				p.api.State = "ready"
			}
		}
	}
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Requests = append(r.Requests, req.Method+" "+req.URL.Path)
	owner, ok := r.accounts[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		problem(w, http.StatusUnauthorized, "unauthorized", "Authentication required", "The bearer token was not recognized.")
		return
	}
	if r.Forbid {
		problem(w, http.StatusForbidden, "forbidden", "Forbidden", "This token does not carry the knowledge:write scope.")
		return
	}
	if rest, ok := strings.CutPrefix(req.URL.Path, "/api/v1/orgs/"); ok {
		r.serveOrg(w, req, owner, rest)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/api/v1/knowledge")
	switch {
	case req.Method == http.MethodPost && path == "/keys/wrap":
		var dk apiclient.KnowledgeDataKey
		if json.NewDecoder(req.Body).Decode(&dk) != nil || len(dk.Key) != packseal.KeySize {
			problem(w, http.StatusUnprocessableEntity, "validation", "The registry refused the key.", "Must be a 32-byte data key, base64.")
			return
		}
		writeJSON(w, http.StatusOK, r.wrap(owner, dk.Key))
	case req.Method == http.MethodPost && path == "/keys/unwrap":
		var wk apiclient.KnowledgeWrappedKey
		_ = json.NewDecoder(req.Body).Decode(&wk)
		k, err := r.unwrap(owner, wk)
		if err != nil {
			keyNotFound(w)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, apiclient.KnowledgeDataKey{Key: k})
	case req.Method == http.MethodGet && path == "/packs":
		out := []apiclient.KnowledgePack{}
		for _, p := range r.packs[owner] {
			out = append(out, p.api)
		}
		writeJSON(w, http.StatusOK, out)
	case req.Method == http.MethodPost && path == "/packs":
		r.create(w, req, owner)
	case strings.HasPrefix(path, "/packs/"):
		r.onePack(w, req, owner, strings.TrimPrefix(path, "/packs/"))
	default:
		problem(w, http.StatusNotFound, "not-found", "Not found", "")
	}
}

func (r *Registry) create(w http.ResponseWriter, req *http.Request, owner string) {
	var body apiclient.CreateKnowledgePackRequest
	if json.NewDecoder(req.Body).Decode(&body) != nil || knowledge.ValidName(body.Name) != nil {
		problem(w, http.StatusUnprocessableEntity, "validation", "The registry refused the pack.", "")
		return
	}
	if _, err := r.unwrap(owner, body.WrappedKey); err != nil {
		keyNotFound(w)
		return
	}
	for _, p := range r.packs[owner] {
		if p.api.Name == body.Name {
			problem(w, http.StatusConflict, "knowledge-name-taken", "Pack name taken", "This account already keeps a knowledge pack of this name here.")
			return
		}
	}
	var id openapi_types.UUID
	_, _ = rand.Read(id[:])
	now := time.Now().UTC()
	p := &fakePack{api: apiclient.KnowledgePack{Id: id, Name: body.Name, State: "empty", WrappedKey: body.WrappedKey, CreatedAt: now, UpdatedAt: now}}
	if r.packs[owner] == nil {
		r.packs[owner] = map[string]*fakePack{}
	}
	r.packs[owner][id.String()] = p
	writeJSON(w, http.StatusCreated, p.api)
}

func (r *Registry) onePack(w http.ResponseWriter, req *http.Request, owner, rest string) {
	id, tail, _ := strings.Cut(rest, "/")
	p := r.packs[owner][id]
	if p == nil {
		problem(w, http.StatusNotFound, "not-found", "Not found", "")
		return
	}
	switch {
	case req.Method == http.MethodPut && tail == "tree":
		r.upload(w, req, owner, p)
	case req.Method == http.MethodGet && tail == "tree":
		if p.api.State == "scanning" {
			problem(w, http.StatusConflict, "knowledge-scanning", "Pack being scanned", "This pack's tree is being scanned; it can be read once the scan ends.")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Agentry-Knowledge-Digest", p.api.Digest)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(p.sealed)
	case req.Method == http.MethodDelete && tail == "":
		delete(r.packs[owner], id)
		w.WriteHeader(http.StatusNoContent)
	default:
		problem(w, http.StatusNotFound, "not-found", "Not found", "")
	}
}

// upload opens and checks the tree as the core does before keeping it.
func (r *Registry) upload(w http.ResponseWriter, req *http.Request, owner string, p *fakePack) {
	sealed, err := io.ReadAll(req.Body)
	if err != nil {
		problem(w, http.StatusBadRequest, "bad-request", "Bad request", "")
		return
	}
	k, err := r.unwrap(owner, p.api.WrappedKey)
	if err != nil {
		keyNotFound(w)
		return
	}
	h, plain, err := packseal.Open(bytes.NewReader(sealed), k)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "knowledge-tree-invalid", "Not this pack's sealed tree", "The upload is not a knowledge pack's tree sealed under this pack's data key; nothing was stored.")
		return
	}
	dir, err := os.MkdirTemp("", "fake-registry-tree-")
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal", "Internal", "")
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	d, err := knowledge.ExtractTree(plain, dir+"/tree", h.Digest)
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "knowledge-digest-mismatch", "Tree does not match its digest", err.Error())
		return
	}
	pages, _ := knowledge.Pages(dir + "/tree")
	var report apiclient.KnowledgeScanReport
	_ = json.Unmarshal([]byte(scanReport), &report)
	p.sealed = sealed
	p.api.Bytes, p.api.Files, p.api.Pages, p.api.Digest = d.Bytes, d.Files, pages, d.SHA256
	p.api.Scan = &report
	p.api.State = "ready"
	if r.Scanning {
		p.api.State = "scanning"
	}
	p.api.UpdatedAt = time.Now().UTC()
	writeJSON(w, http.StatusAccepted, p.api)
}

func (r *Registry) wrap(owner string, dk []byte) apiclient.KnowledgeWrappedKey {
	aead := r.aead()
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return apiclient.KnowledgeWrappedKey{Kind: "deployment", Recipient: "deployment:fake", Key: aead.Seal(nonce, nonce, dk, []byte(owner))}
}

func (r *Registry) unwrap(owner string, wk apiclient.KnowledgeWrappedKey) ([]byte, error) {
	aead := r.aead()
	if wk.Kind != "deployment" || len(wk.Key) < aead.NonceSize() {
		return nil, fmt.Errorf("not a wrapped key")
	}
	return aead.Open(nil, wk.Key[:aead.NonceSize()], wk.Key[aead.NonceSize():], []byte(owner))
}

func (r *Registry) aead() cipher.AEAD {
	block, _ := aes.NewCipher(r.key)
	g, _ := cipher.NewGCM(block)
	return g
}

func keyNotFound(w http.ResponseWriter) {
	problem(w, http.StatusNotFound, "knowledge-key-not-found", "Wrapped key not found", "This wrapped key does not open for this account.")
}

func problem(w http.ResponseWriter, status int, slug, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://agentry.dev/problems/" + slug, "title": title, "status": status, "detail": detail})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// org is the organization named org, made the first time it is asked for.
func (r *Registry) org(org string) *fakeOrg {
	o := r.orgs[org]
	if o == nil {
		o = &fakeOrg{members: map[string]string{}, packs: map[string]*fakePack{}}
		r.orgs[org] = o
	}
	return o
}

// Join makes owner an active member of org, as role ("admin" or "member").
func (r *Registry) Join(org, owner, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.org(org).members[owner] = role
}

// Remove ends owner's membership of org: every later call of theirs to the
// organization's routes is a 404, as the core answers.
func (r *Registry) Remove(org, owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.org(org).members, owner)
}

// SeedShared makes a ready shared pack of org named name holding files
// (paths under wiki/ and sources/), sealed under a fresh data key wrapped
// for the organization, and answers its registry id.
func (r *Registry) SeedShared(t testing.TB, org, name string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		full := dir + "/" + p
		if err := os.MkdirAll(full[:strings.LastIndex(full, "/")], 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var tree bytes.Buffer
	d, err := knowledge.WriteTree(dir, &tree)
	if err != nil {
		t.Fatal(err)
	}
	pages, _ := knowledge.Pages(dir)
	dk, err := packseal.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	var idb [16]byte
	_, _ = rand.Read(idb[:])
	packHeader := fmt.Sprintf("kp_%x", idb)
	var sealed bytes.Buffer
	if err := packseal.Seal(&sealed, dk, packseal.Header{Pack: packHeader, Digest: d.SHA256}, bytes.NewReader(tree.Bytes())); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var id openapi_types.UUID
	_, _ = rand.Read(id[:])
	now := time.Now().UTC()
	var report apiclient.KnowledgeScanReport
	_ = json.Unmarshal([]byte(scanReport), &report)
	no := false
	orgName := org
	p := &fakePack{sealed: sealed.Bytes(), tree: tree.Bytes(), api: apiclient.KnowledgePack{
		Id: id, Name: name, State: "ready", Bytes: d.Bytes, Files: d.Files, Pages: pages, Digest: d.SHA256,
		WrappedKey: r.wrap(orgOwner(org), dk), Scan: &report, CreatedAt: now, UpdatedAt: now,
		Shared: true, Org: &orgName, Exportable: &no,
	}}
	r.org(org).packs[id.String()] = p
	return id.String()
}

// SetExportable marks org's pack id exportable by its members, or not.
func (r *Registry) SetExportable(org, id string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.org(org).packs[id]; p != nil {
		v := on
		p.api.Exportable = &v
	}
}

// orgOwner binds an organization's wrapped keys to it, apart from any
// account's.
func orgOwner(org string) string { return "org:" + org }

// serveOrg answers /api/v1/orgs/<org>/knowledge/…, for org's members.
func (r *Registry) serveOrg(w http.ResponseWriter, req *http.Request, owner, rest string) {
	org, path, ok := strings.Cut(rest, "/knowledge")
	o := r.orgs[org]
	role := ""
	if o != nil {
		role = o.members[owner]
	}
	if !ok || role == "" {
		problem(w, http.StatusNotFound, "not-found", "Not found", "")
		return
	}
	switch {
	case req.Method == http.MethodPost && path == "/keys/unwrap":
		var wk apiclient.KnowledgeWrappedKey
		_ = json.NewDecoder(req.Body).Decode(&wk)
		k, err := r.unwrap(orgOwner(org), wk)
		if err != nil {
			keyNotFound(w)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, apiclient.KnowledgeDataKey{Key: k})
	case req.Method == http.MethodGet && path == "/packs":
		out := []apiclient.KnowledgePack{}
		for _, p := range o.packs {
			out = append(out, p.api)
		}
		writeJSON(w, http.StatusOK, out)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/packs/"):
		id, tail, _ := strings.Cut(strings.TrimPrefix(path, "/packs/"), "/")
		p := o.packs[id]
		if p == nil {
			problem(w, http.StatusNotFound, "not-found", "Not found", "")
			return
		}
		switch tail {
		case "":
			writeJSON(w, http.StatusOK, p.api)
		case "tree":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Agentry-Knowledge-Digest", p.api.Digest)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(p.sealed)
		case "export":
			if role != "admin" && (p.api.Exportable == nil || !*p.api.Exportable) {
				problem(w, http.StatusForbidden, "knowledge-not-exportable", "Pack not exportable", "Only the organization's admins export this pack unencrypted. An admin can mark it exportable.")
				return
			}
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("X-Agentry-Knowledge-Digest", p.api.Digest)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(p.tree)
		default:
			problem(w, http.StatusNotFound, "not-found", "Not found", "")
		}
	default:
		problem(w, http.StatusNotFound, "not-found", "Not found", "")
	}
}

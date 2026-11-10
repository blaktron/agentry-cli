package managed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/configdir"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// mapKeychain is a keychain in memory.
type mapKeychain struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *mapKeychain) Get(a string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[a]
	if !ok {
		return "", modelkey.ErrNotStored
	}
	return v, nil
}
func (k *mapKeychain) Set(a, v string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[a] = v
	return nil
}
func (k *mapKeychain) Delete(a string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.m[a]; !ok {
		return modelkey.ErrNotStored
	}
	delete(k.m, a)
	return nil
}

// keyRegistry is a registry that delivers the organization's keys by id:
// it seals each to the public key the request carries, bound as the
// registry would bind it.
type keyRegistry struct {
	srv     *httptest.Server
	keys    map[string]string // key id -> value
	mu      sync.Mutex
	pending map[string]managedsettings.Delivery
	pubs    map[string][]byte
	asked   int
}

const keyAccountID = "6b1f0e7a-0000-4000-8000-000000000001"

func newKeyRegistry(t *testing.T, org string, keys map[string]string) *keyRegistry {
	t.Helper()
	r := &keyRegistry{keys: keys, pending: map[string]managedsettings.Delivery{}, pubs: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer member-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/api/v1/x/vault/me/org-keys/"):
			key := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/api/v1/x/vault/me/org-keys/"), "/deliveries")
			var body struct {
				Rev       int64
				PublicKey string
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			pub, _ := base64.StdEncoding.DecodeString(body.PublicKey)
			r.mu.Lock()
			r.asked++
			id := "d" + string(rune('0'+r.asked))
			r.pending[id] = managedsettings.Delivery{Registry: "https://reg.example", Org: org, Account: keyAccountID, Key: key, Rev: body.Rev}
			r.pubs[id] = pub
			r.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "state": "pending"})
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/x/vault/me/org-key-deliveries/"):
			id := strings.TrimPrefix(req.URL.Path, "/api/v1/x/vault/me/org-key-deliveries/")
			r.mu.Lock()
			d, pub := r.pending[id], r.pubs[id]
			r.mu.Unlock()
			sealed, err := managedsettings.SealDelivery(pub, d, []byte(r.keys[d.Key]))
			if err != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "sealed", "sealed": base64.StdEncoding.EncodeToString(sealed)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func reviewDoc(org, key string, rev int64) managedsettings.Document {
	v, _ := json.Marshal(managedsettings.Source{Source: managedsettings.SourceOrganizationKey, Key: &managedsettings.KeyRef{ID: key, Rev: rev}})
	d := docFor("https://reg.example", rev, map[string]managedsettings.Setting{managedsettings.ReviewSource: {Value: v, Locked: true}})
	d.Org = org
	return d
}

func TestADeliveredKeyIsFetchedOncePerRevisionAndGoesWithTheDocument(t *testing.T) {
	t.Setenv(configdir.EnvVar, t.TempDir())
	kc := &mapKeychain{m: map[string]string{}}
	defer modelkey.UseKeychain(kc)()
	const key = "5e0c3a10-0000-4000-8000-0000000000aa"
	r := newKeyRegistry(t, "acme", map[string]string{key: "ts-acme-review-1"})
	reg := normalize("https://reg.example")
	ctx := context.Background()

	if err := syncKeys(ctx, r.srv.Client(), r.srv.URL, "member-token", keyAccountID, reviewDoc("acme", key, 1)); err != nil {
		t.Fatal(err)
	}
	// syncKeys was asked against the server's address; the record is kept
	// under the registry it was asked for.
	k, _ := readKeys(r.srv.URL)
	if len(k.Keys) != 1 || !k.Keys[0].Held || k.Keys[0].Account != "managed:acme:typesafe" || k.Keys[0].Rev != 1 {
		t.Fatalf("record %+v", k)
	}
	if kc.m["managed:acme:typesafe"] != "ts-acme-review-1" {
		t.Fatal("the key was not kept under managed:acme:typesafe")
	}
	raw, _ := json.Marshal(k)
	if strings.Contains(string(raw), "ts-acme-review-1") {
		t.Fatal("the record holds the key's value")
	}
	// The same revision again asks nothing.
	if err := syncKeys(ctx, r.srv.Client(), r.srv.URL, "member-token", keyAccountID, reviewDoc("acme", key, 1)); err != nil || r.asked != 1 {
		t.Fatalf("a held revision was asked again: %d (%v)", r.asked, err)
	}
	// A new revision is fetched and replaces it.
	r.keys[key] = "ts-acme-review-2"
	if err := syncKeys(ctx, r.srv.Client(), r.srv.URL, "member-token", keyAccountID, reviewDoc("acme", key, 2)); err != nil || r.asked != 2 || kc.m["managed:acme:typesafe"] != "ts-acme-review-2" {
		t.Fatalf("a new revision: %d asks, %q (%v)", r.asked, kc.m["managed:acme:typesafe"], err)
	}
	// A document that no longer names it deletes it; so does removing the
	// document.
	if err := syncKeys(ctx, r.srv.Client(), r.srv.URL, "member-token", keyAccountID, docFor("https://reg.example", 3, nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := kc.m["managed:acme:typesafe"]; ok {
		t.Fatal("a key the document no longer names was kept")
	}
	_ = syncKeys(ctx, r.srv.Client(), r.srv.URL, "member-token", keyAccountID, reviewDoc("acme", key, 2))
	if err := removeDocument(r.srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, ok := kc.m["managed:acme:typesafe"]; ok {
		t.Fatal("removing the document left its key")
	}
	_ = reg
}

func TestAKeyThatCouldNotBeDeliveredIsRecordedNotFatal(t *testing.T) {
	t.Setenv(configdir.EnvVar, t.TempDir())
	kc := &mapKeychain{m: map[string]string{}}
	defer modelkey.UseKeychain(kc)()
	r := newKeyRegistry(t, "acme", nil)
	if err := syncKeys(context.Background(), r.srv.Client(), r.srv.URL, "wrong-token", keyAccountID, reviewDoc("acme", "5e0c3a10-0000-4000-8000-0000000000bb", 1)); err != nil {
		t.Fatal(err)
	}
	k, _ := readKeys(r.srv.URL)
	if len(k.Keys) != 1 || k.Keys[0].Held || !strings.Contains(k.Keys[0].Err, "HTTP 401") {
		t.Fatalf("record %+v", k)
	}
}

func TestTheSourceRowsDecideWhichKeyAServiceUses(t *testing.T) {
	src := func(s string, locked bool) map[string]managedsettings.Setting {
		v, _ := json.Marshal(managedsettings.Source{Source: s})
		return map[string]managedsettings.Setting{managedsettings.ReviewSource: {Value: v, Locked: locked}}
	}
	if _, decided, err := held("acme", src("member", true)).ServiceKey(managedsettings.ReviewSource, policy.JudgeKeyProvider, "own"); decided || err != nil {
		t.Fatal("member: the member's own key stands")
	}
	if _, decided, err := held("acme", src("off", true)).ServiceKey(managedsettings.ReviewSource, policy.JudgeKeyProvider, "own"); !decided || err == nil || !strings.Contains(err.Error(), "AI Review is off by acme's managed settings (review.source)") {
		t.Fatalf("a lock to off: %v", err)
	}
	if _, decided, err := held("acme", src("off", false)).ServiceKey(managedsettings.ReviewSource, policy.JudgeKeyProvider, "own"); decided || err != nil {
		t.Fatal("a default off gives way to the member's own key")
	}
	if _, _, err := held("acme", src("agentry-credits", true)).ServiceKey(managedsettings.ReviewSource, policy.JudgeKeyProvider, ""); err == nil || !strings.Contains(err.Error(), "update Agentry") {
		t.Fatalf("a lock to credits this version cannot pay with: %v", err)
	}
}

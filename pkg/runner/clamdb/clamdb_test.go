package clamdb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// cvd is a CVD with a real header and a body that stands in for the
// signed archive.
func cvd(t *testing.T, built string, version int, body string) []byte {
	t.Helper()
	h := fmt.Sprintf("ClamAV-VDB:%s:%d:100:90:md5:dsig:builder:0", built, version)
	b := make([]byte, cvdHeaderSize)
	copy(b, h)
	for i := len(h); i < cvdHeaderSize; i++ {
		b[i] = ' '
	}
	return append(b, body...)
}

// fakeRegistry serves one repository as GHCR does: a bearer challenge
// answered by an anonymous token, manifests by tag and digest, cosign's
// referrers under the sha256-<hex> tag, and blobs. It counts requests.
type fakeRegistry struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	// manifests and blobs by tag or digest.
	manifests map[string][]byte
	blobs     map[string][]byte
	requests  map[string]int
	// signed is the digests verifySig accepts.
	signed map[string]bool
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	r := &fakeRegistry{t: t, manifests: map[string][]byte{}, blobs: map[string][]byte{}, requests: map[string]int{}, signed: map[string]bool{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path == "/token" {
		if req.URL.Query().Get("scope") != "repository:blaktron/agentry-clamav-db:pull" {
			http.Error(w, "scope", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"token":"anon"}`)
		return
	}
	if req.Header.Get("Authorization") != "Bearer anon" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="ghcr.io",scope="repository:blaktron/agentry-clamav-db:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	const prefix = "/v2/blaktron/agentry-clamav-db/"
	rest, ok := strings.CutPrefix(req.URL.Path, prefix)
	if !ok {
		http.NotFound(w, req)
		return
	}
	kind, ref, _ := strings.Cut(rest, "/")
	r.requests[kind+"/"+ref]++
	var body []byte
	switch kind {
	case "manifests":
		body = r.manifests[ref]
	case "blobs":
		body = r.blobs[ref]
	}
	if body == nil {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(body)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *fakeRegistry) blob(b []byte) descriptor {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := digestOf(b)
	r.blobs[d] = b
	return descriptor{Digest: d, Size: int64(len(b))}
}

// publish pushes a bundle of the three databases under latest, signed
// unless unsigned, as the clamav-db workflow does, and answers its digest.
func (r *fakeRegistry) publish(dbs map[string][]byte, unsigned bool) string {
	m := manifestDoc{MediaType: ociManifest, ArtifactType: bundleType, Config: descriptor{MediaType: "application/vnd.oci.empty.v1+json"}}
	for _, name := range databases {
		d := r.blob(dbs[name])
		d.MediaType, d.Annotations = cvdType, map[string]string{titleKey: name}
		m.Layers = append(m.Layers, d)
	}
	return r.publishManifest(m, unsigned)
}

func (r *fakeRegistry) publishManifest(m manifestDoc, unsigned bool) string {
	body, err := json.Marshal(m)
	if err != nil {
		r.t.Fatal(err)
	}
	digest := digestOf(body)
	sig := r.blob([]byte("signature over " + digest))
	sig.MediaType = sigstoreType
	sigManifest, _ := json.Marshal(manifestDoc{MediaType: ociManifest, ArtifactType: sigstoreType, Layers: []descriptor{sig}})
	index, _ := json.Marshal(manifestDoc{MediaType: ociIndex, Manifests: []descriptor{{MediaType: ociManifest, Digest: digestOf(sigManifest), ArtifactType: "application/vnd.oci.empty.v1+json"}}})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests["latest"] = body
	r.manifests[digest] = body
	r.manifests[digestOf(sigManifest)] = sigManifest
	r.manifests["sha256-"+strings.TrimPrefix(digest, "sha256:")] = index
	if !unsigned {
		r.signed[digest] = true
	}
	return digest
}

func (r *fakeRegistry) count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, v := range r.requests {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}

func (r *fakeRegistry) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = map[string]int{}
}

func (r *fakeRegistry) ref() string {
	return strings.TrimPrefix(r.srv.URL, "http://") + "/blaktron/agentry-clamav-db:latest"
}

// setup points the package at the fake: plain HTTP, a clock the test
// moves, and a signature check that accepts what the fake signed (the
// real check has its own tests).
func setup(t *testing.T) (*fakeRegistry, *time.Time) {
	t.Helper()
	r := newFakeRegistry(t)
	clock := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	oldNow, oldScheme, oldRoot, oldVerify := now, scheme, trustedRoot, verifySig
	now = func() time.Time { return clock }
	scheme = "http"
	trustedRoot = func(context.Context, string) (root.TrustedMaterial, error) { return nil, nil }
	verifySig = func(_ root.TrustedMaterial, raw []byte, digest string) error {
		if !r.signed[digest] || string(raw) != "signature over "+digest {
			return errors.New("not signed")
		}
		return nil
	}
	t.Cleanup(func() { now, scheme, trustedRoot, verifySig = oldNow, oldScheme, oldRoot, oldVerify })
	return r, &clock
}

func dbs(t *testing.T, daily, main int, dailyBuilt string) map[string][]byte {
	return map[string][]byte{
		"main.cvd":     cvd(t, "16 Dec 2025 23-18 +0000", main, "main signatures"),
		"daily.cvd":    cvd(t, dailyBuilt, daily, fmt.Sprintf("daily signatures %d", daily)),
		"bytecode.cvd": cvd(t, "11 Sep 2025 08-29 -0400", 339, "bytecode"),
	}
}

func TestEnsureFetchesVerifiesAndRefreshesDaily(t *testing.T) {
	r, clock := setup(t)
	home := t.TempDir()
	var log bytes.Buffer
	first := r.publish(dbs(t, 28143, 63, "05 Oct 2026 06-25 +0000"), false)

	db := Ensure(context.Background(), home, r.ref(), &log)
	if db.Dir == "" || db.Problem != "" || db.Digest != first || db.Stale {
		t.Fatalf("first Ensure = %+v\n%s", db, log.String())
	}
	if db.Daily.Number != 28143 || db.Main.Number != 63 || !db.Daily.Built.Equal(time.Date(2026, 10, 5, 6, 25, 0, 0, time.UTC)) {
		t.Errorf("versions = %+v %+v", db.Daily, db.Main)
	}
	for _, name := range databases {
		if fi, err := os.Stat(filepath.Join(db.Dir, name)); err != nil || fi.Mode().Perm()&0o044 != 0o044 {
			t.Errorf("%s is not there, readable by the decode container: %v", name, err)
		}
	}

	t.Run("within a day the registry is not asked", func(t *testing.T) {
		r.reset()
		*clock = clock.Add(23 * time.Hour)
		if again := Ensure(context.Background(), home, r.ref(), io.Discard); again.Dir != db.Dir || r.count("") != 0 {
			t.Errorf("Ensure asked the registry %d times within the day: %+v", r.count(""), again)
		}
	})

	t.Run("after a day the same bundle moves only the clock", func(t *testing.T) {
		r.reset()
		*clock = clock.Add(2 * time.Hour)
		again := Ensure(context.Background(), home, r.ref(), io.Discard)
		if again.Dir != db.Dir || r.count("blobs/") != 0 || r.count("manifests/latest") != 1 || !again.Checked.Equal(*clock) {
			t.Errorf("a same-digest refresh = %+v; requests %v", again, r.requests)
		}
	})

	t.Run("a new bundle downloads only what changed", func(t *testing.T) {
		r.reset()
		*clock = clock.Add(25 * time.Hour)
		second := r.publish(dbs(t, 28144, 63, "06 Oct 2026 06-25 +0000"), false)
		next := Ensure(context.Background(), home, r.ref(), io.Discard)
		if next.Digest != second || next.Daily.Number != 28144 || next.Problem != "" {
			t.Fatalf("the second bundle = %+v", next)
		}
		if got := r.count("blobs/"); got != 2 { // daily.cvd and the signature
			t.Errorf("%d blobs fetched, want 2 (daily.cvd and the signature): %v", got, r.requests)
		}
		if b, err := os.ReadFile(filepath.Join(next.Dir, "main.cvd")); err != nil || !bytes.Contains(b, []byte("main signatures")) {
			t.Errorf("main.cvd was not carried over: %v", err)
		}
		if _, err := os.Stat(db.Dir); err != nil {
			t.Errorf("the previous bundle, which a decode may still read, was removed: %v", err)
		}
	})
}

func TestEnsureRefusesWhatItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		name    string
		publish func(r *fakeRegistry, t *testing.T)
	}{
		{"unsigned", func(r *fakeRegistry, t *testing.T) { r.publish(dbs(t, 28144, 63, "06 Oct 2026 06-25 +0000"), true) }},
		{"no signature at all", func(r *fakeRegistry, t *testing.T) {
			d := r.publish(dbs(t, 28144, 63, "06 Oct 2026 06-25 +0000"), false)
			delete(r.manifests, "sha256-"+strings.TrimPrefix(d, "sha256:"))
		}},
		{"older than the one held", func(r *fakeRegistry, t *testing.T) { r.publish(dbs(t, 28100, 63, "01 Oct 2026 06-25 +0000"), false) }},
		{"a tampered layer", func(r *fakeRegistry, t *testing.T) {
			r.publish(dbs(t, 28144, 63, "06 Oct 2026 06-25 +0000"), false)
			for d, b := range r.blobs {
				if bytes.Contains(b, []byte("daily signatures 28144")) {
					r.blobs[d] = bytes.Replace(b, []byte("28144"), []byte("66666"), 1)
				}
			}
		}},
		{"a fourth layer", func(r *fakeRegistry, t *testing.T) {
			m := manifestDoc{MediaType: ociManifest, ArtifactType: bundleType}
			for i, name := range []string{"main.cvd", "daily.cvd", "bytecode.cvd", "extra.cvd"} {
				d := r.blob(cvd(t, "06 Oct 2026 06-25 +0000", 28144+i, name))
				d.MediaType, d.Annotations = cvdType, map[string]string{titleKey: name}
				m.Layers = append(m.Layers, d)
			}
			r.publishManifest(m, false)
		}},
		{"another artifact", func(r *fakeRegistry, t *testing.T) {
			m := manifestDoc{MediaType: ociManifest, ArtifactType: "application/vnd.example.other"}
			for _, name := range databases {
				d := r.blob(cvd(t, "06 Oct 2026 06-25 +0000", 28144, name))
				d.MediaType, d.Annotations = cvdType, map[string]string{titleKey: name}
				m.Layers = append(m.Layers, d)
			}
			r.publishManifest(m, false)
		}},
		{"a layer named outside the three", func(r *fakeRegistry, t *testing.T) {
			m := manifestDoc{MediaType: ociManifest, ArtifactType: bundleType}
			for _, name := range []string{"main.cvd", "daily.cvd", "../evil.cvd"} {
				d := r.blob(cvd(t, "06 Oct 2026 06-25 +0000", 28144, name))
				d.MediaType, d.Annotations = cvdType, map[string]string{titleKey: name}
				m.Layers = append(m.Layers, d)
			}
			r.publishManifest(m, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, clock := setup(t)
			home := t.TempDir()
			held := r.publish(dbs(t, 28143, 63, "05 Oct 2026 06-25 +0000"), false)
			if db := Ensure(context.Background(), home, r.ref(), io.Discard); db.Digest != held {
				t.Fatalf("the first bundle = %+v", db)
			}
			*clock = clock.Add(25 * time.Hour)
			tc.publish(r, t)
			var log bytes.Buffer
			db := Ensure(context.Background(), home, r.ref(), &log)
			if db.Digest != held || db.Problem != ProblemRefresh || db.Dir == "" {
				t.Errorf("Ensure took a bundle it cannot trust: %+v", db)
			}
			if !strings.Contains(log.String(), "could not refresh") {
				t.Errorf("the refusal was not logged: %q", log.String())
			}
			entries, _ := os.ReadDir(filepath.Join(home, "clamav", "db"))
			if len(entries) != 1 {
				t.Errorf("the refused bundle left %d directories, want only the one held", len(entries))
			}
		})
	}
}

func TestEnsureWithNoBundleChecksNothingAndSaysWhy(t *testing.T) {
	r, _ := setup(t)
	r.publish(dbs(t, 28143, 63, "05 Oct 2026 06-25 +0000"), true)
	db := Ensure(context.Background(), t.TempDir(), r.ref(), io.Discard)
	if db.Dir != "" || db.Problem != ProblemNone {
		t.Errorf("Ensure with nothing it trusts = %+v, want no directory and %q", db, ProblemNone)
	}
	if db := Ensure(context.Background(), "", r.ref(), io.Discard); db.Dir != "" || db.Problem != ProblemNone {
		t.Errorf("Ensure with no runner home = %+v", db)
	}
}

func TestSignaturesPastSevenDaysAreStale(t *testing.T) {
	r, clock := setup(t)
	home := t.TempDir()
	r.publish(dbs(t, 28143, 63, "05 Oct 2026 06-25 +0000"), false)
	if db := Ensure(context.Background(), home, r.ref(), io.Discard); db.Stale {
		t.Fatalf("fresh signatures called stale: %+v", db)
	}
	// Eight days on, the registry is gone: the bundle held is used, named
	// stale, and the failed refresh is said.
	r.srv.Close()
	*clock = clock.Add(8 * 24 * time.Hour)
	db := Ensure(context.Background(), home, r.ref(), io.Discard)
	if db.Dir == "" || !db.Stale || db.Problem != ProblemRefresh {
		t.Errorf("eight days on, offline = %+v, want the held bundle, stale, refresh failed", db)
	}
}

func TestParseCVDHeader(t *testing.T) {
	v, err := parseCVDHeader(cvd(t, "04 Oct 2026 06-25 +0000", 28143, "")[:cvdHeaderSize])
	if err != nil || v.Number != 28143 || !v.Built.Equal(time.Date(2026, 10, 4, 6, 25, 0, 0, time.UTC)) {
		t.Errorf("= %+v, %v", v, err)
	}
	v, err = parseCVDHeader(cvd(t, "11 Sep 2025 08-29 -0400", 339, "")[:cvdHeaderSize])
	if err != nil || !v.Built.Equal(time.Date(2025, 9, 11, 12, 29, 0, 0, time.UTC)) {
		t.Errorf("an offset build time = %+v, %v", v, err)
	}
	for _, bad := range []string{"PK\x03\x04", "ClamAV-VDB:yesterday:1:2:3", "ClamAV-VDB:04 Oct 2026 06-25 +0000:x:2:3", "ClamAV-VDB:04 Oct 2026 06-25 +0000"} {
		if _, err := parseCVDHeader([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestValidRef(t *testing.T) {
	for ref, want := range map[string]bool{
		DefaultRef: true,
		"localhost:5000/blaktron/agentry-clamav-db:latest":                     true,
		"ghcr.io/blaktron/agentry-clamav-db":                                   false,
		"ghcr.io/blaktron/agentry-clamav-db@sha256:" + strings.Repeat("a", 64): false,
		"https://ghcr.io/x:latest":                                             false,
	} {
		if got := ValidRef(ref); got != want {
			t.Errorf("ValidRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestTheDefaultIsThePolicysDefault(t *testing.T) {
	if DefaultRef != policy.DefaultClamAVSignatures || !ValidRef(policy.DefaultClamAVSignatures) {
		t.Errorf("clamdb.DefaultRef %q and policy.DefaultClamAVSignatures %q differ", DefaultRef, policy.DefaultClamAVSignatures)
	}
}

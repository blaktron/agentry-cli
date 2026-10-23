package osvdb

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// bucket stands in for OSV's: each ecosystem's all.zip, a count of the
// requests, and an answer it can be told to break.
type bucket struct {
	mu       sync.Mutex
	requests map[string]int
	broken   map[string]string // ecosystem → "500" or "notzip"
}

func zipOf(t *testing.T, name string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create(name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`{"id":"` + name + `"}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func serve(t *testing.T) *bucket {
	t.Helper()
	bk := &bucket{requests: map[string]int{}, broken: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eco, file, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if !ok || file != "all.zip" {
			http.NotFound(w, r)
			return
		}
		bk.mu.Lock()
		bk.requests[eco]++
		broken := bk.broken[eco]
		bk.mu.Unlock()
		switch broken {
		case "500":
			http.Error(w, "down", http.StatusInternalServerError)
		case "notzip":
			_, _ = w.Write([]byte("<html>not an archive</html>"))
		default:
			_, _ = w.Write(zipOf(t, eco))
		}
	}))
	t.Cleanup(srv.Close)
	oldURL, oldNow := baseURL, now
	baseURL = srv.URL
	t.Cleanup(func() { baseURL, now = oldURL, oldNow })
	return bk
}

func TestEnsureKeepsADailyCopyInOSVScannersLayout(t *testing.T) {
	bk := serve(t)
	home := t.TempDir()
	clock := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	db := Ensure(context.Background(), home, io.Discard)
	if !db.Held() || db.Problem != "" || len(db.Fetched) != len(Ecosystems) || db.Dir != Dir(home) {
		t.Fatalf("first Ensure: %+v", db)
	}
	for _, e := range Ecosystems {
		p := filepath.Join(home, "osv", "osv-scalibr", e, "all.zip")
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o644 {
			t.Fatalf("%s: %v %v", p, err, fi)
		}
	}
	// Within a day nothing is asked again.
	clock = clock.Add(23 * time.Hour)
	Ensure(context.Background(), home, io.Discard)
	if bk.requests["Go"] != 1 {
		t.Fatalf("asked %d times within a day", bk.requests["Go"])
	}
	// After a day each is downloaded again.
	clock = clock.Add(2 * time.Hour)
	Ensure(context.Background(), home, io.Discard)
	if bk.requests["Go"] != 2 || bk.requests["npm"] != 2 {
		t.Fatalf("after a day: %v", bk.requests)
	}
}

// TestARefreshThatFailsKeepsTheCopy: an archive the bucket will not give,
// or gives broken, leaves the one held in place and names the failure; it
// is not asked again for an hour; with no copy at all there is none to
// mount.
func TestARefreshThatFailsKeepsTheCopy(t *testing.T) {
	bk := serve(t)
	home := t.TempDir()
	clock := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	Ensure(context.Background(), home, io.Discard)
	before, err := os.ReadFile(filepath.Join(home, "osv", "osv-scalibr", "npm", "all.zip"))
	if err != nil {
		t.Fatal(err)
	}
	bk.broken["npm"], bk.broken["Go"] = "notzip", "500"
	clock = clock.Add(25 * time.Hour)
	db := Ensure(context.Background(), home, io.Discard)
	after, _ := os.ReadFile(filepath.Join(home, "osv", "osv-scalibr", "npm", "all.zip"))
	if db.Problem != ProblemRefresh || !db.Held() || !bytes.Equal(before, after) || len(db.Fetched) != len(Ecosystems) {
		t.Fatalf("a failed refresh: %+v", db)
	}
	asked := bk.requests["npm"]
	clock = clock.Add(30 * time.Minute)
	if db := Ensure(context.Background(), home, io.Discard); db.Problem != ProblemRefresh || bk.requests["npm"] != asked {
		t.Fatalf("retried within the hour: %+v, %d requests", db, bk.requests["npm"])
	}
	// A machine with nothing held and nothing to download has none.
	bare := t.TempDir()
	for _, e := range Ecosystems {
		bk.broken[e] = "500"
	}
	if db := Ensure(context.Background(), bare, io.Discard); db.Held() || db.Problem != ProblemNone {
		t.Fatalf("with nothing: %+v", db)
	}
	if entries, _ := os.ReadDir(filepath.Join(bare, "osv", "osv-scalibr", "Go")); len(entries) != 0 {
		t.Fatalf("a failed download left %v", entries)
	}
}

func TestStaleNamesArchivesAWeekOld(t *testing.T) {
	old := now
	t.Cleanup(func() { now = old })
	clock := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	now = func() time.Time { return clock }
	db := DB{Dir: "x", Fetched: map[string]time.Time{"Go": clock.Add(-8 * 24 * time.Hour), "npm": clock.Add(-time.Hour)}}
	if s := db.Stale(); len(s) != 1 || s[0] != "Go" {
		t.Fatalf("stale %v", s)
	}
}

// TestARedirectIsNotFollowed: an archive comes from OSV's bucket alone.
func TestARedirectIsNotFollowed(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(zipOf(t, "elsewhere")) }))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = old })
	if db := Ensure(context.Background(), t.TempDir(), io.Discard); db.Held() {
		t.Fatalf("a redirected archive was kept: %+v", db)
	}
}

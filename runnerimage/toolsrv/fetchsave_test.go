package toolsrv

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// pngFixture is a binary file that sniffs as a PNG: the signature, then
// bytes that are not text, larger than several result pages.
func pngFixture() []byte {
	r := rand.New(rand.NewPCG(1, 2))
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	for len(b) < 150<<10 {
		b = append(b, byte(r.UintN(256)))
	}
	return b
}

// saveToFixture is a server for the saveTo tests and the spec of a run that
// may fetch from it, with a scratch folder and a fetch ledger.
func saveToFixture(t *testing.T, secret string) (*httptest.Server, *Spec, *atomic.Bool) {
	t.Helper()
	png := pngFixture()
	var hitOther atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOther.Store(true)
		_, _ = w.Write([]byte("from an undeclared host"))
	}))
	t.Cleanup(other.Close)
	ou, _ := url.Parse(other.URL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/logo.png":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", strconv.Itoa(len(png)))
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Last-Modified", "Sat, 10 Oct 2026 12:00:00 GMT")
			_, _ = w.Write(png)
		case "/notes.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("first line\nthe Needle is here\nlast line\n"))
		case "/secret.bin":
			// The secret's value straddles the copy's 32 KiB parts.
			body := bytes.Repeat([]byte{0}, 32<<10-5)
			_, _ = w.Write(append(append(body, secret...), 0, 1, 2))
		case "/auth":
			_, _ = fmt.Fprintf(w, "auth=%q", r.Header.Get("Authorization"))
		case "/away":
			http.Redirect(w, r, "http://localhost:"+ou.Port()+"/x", http.StatusFound)
		case "/missing":
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	spec := fetchSpecFor(t, srv, "SAVE_TEST_SECRET")
	spec.ScratchDir = t.TempDir()
	spec.Fetch.Ledger = filepath.Join(spec.OutDir, "receipts", "fetches.ndjson")
	return srv, spec, &hitOther
}

func readSaves(t *testing.T, spec *Spec) []SaveEntry {
	t.Helper()
	b, err := os.ReadFile(spec.Fetch.Ledger)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []SaveEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e SaveEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("ledger line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// TestFetchSaveTo: a body of any type from a declared host is saved whole
// at the path asked, never executable, and answered with what was saved,
// never the body; the save is recorded in the fetch ledger.
func TestFetchSaveTo(t *testing.T) {
	const secret = "s4ve-to-secret-value-0123456789"
	t.Setenv("SAVE_TEST_SECRET", secret)
	srv, spec, _ := saveToFixture(t, secret)
	s := newTestServer(t, spec)
	png := pngFixture()

	text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/logo.png", "saveTo": "assets/logo.png"}))
	sum := sha256.Sum256(png)
	want := fmt.Sprintf("HTTP 200 OK\ncontent-type: image/png\ncontent-length: %d\netag: \"v1\"\nlast-modified: Sat, 10 Oct 2026 12:00:00 GMT\n\nsaved: %s/assets/logo.png, %d bytes, sha256 %s;", len(png), spec.ScratchDir, len(png), hex.EncodeToString(sum[:]))
	if isErr || !strings.HasPrefix(text, want) {
		t.Fatalf("(%q, %v)\nwant prefix %q", text, isErr, want)
	}
	if len(text) > 1024 {
		t.Fatalf("the answer holds the body: %d bytes", len(text))
	}
	saved := filepath.Join(spec.ScratchDir, "assets", "logo.png")
	got, err := os.ReadFile(saved)
	if err != nil || !bytes.Equal(got, png) {
		t.Fatalf("the saved file is %d bytes (%v), not the %d fetched", len(got), err, len(png))
	}
	if fi, _ := os.Stat(saved); fi.Mode().Perm()&0o111 != 0 {
		t.Fatalf("the saved file is executable: %v", fi.Mode())
	}
	if entries, _ := os.ReadDir(filepath.Join(spec.ScratchDir, "assets")); len(entries) != 1 {
		t.Fatalf("the save left %d files beside the one asked", len(entries))
	}

	// A 4xx is saved too; its status says what it is.
	if text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/missing", "saveTo": "missing.html"})); isErr || !strings.HasPrefix(text, "HTTP 404 Not Found\n") {
		t.Fatalf("a 404: (%q, %v)", text, isErr)
	}

	// The credential reaches the host through auth alone, and the saved
	// file holds the host's echo of it, which is a run secret: refused.
	if text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/auth", "saveTo": "auth.txt", "headers": map[string]string{"Authorization": "Bearer x"}})); !isErr || !strings.Contains(text, "auth parameter") {
		t.Fatalf("a credential header with saveTo: (%q, %v)", text, isErr)
	}
	if text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/auth", "saveTo": "auth.txt", "auth": map[string]any{"env": "SAVE_TEST_SECRET"}})); !isErr || !strings.Contains(text, "holds the value of this run's secret SAVE_TEST_SECRET") || strings.Contains(text, secret) {
		t.Fatalf("an echoed credential: (%q, %v)", text, isErr)
	}
	if _, err := os.Stat(filepath.Join(spec.ScratchDir, "auth.txt")); !os.IsNotExist(err) {
		t.Fatalf("a refused save left its file: %v", err)
	}

	// Without saveTo, a body larger than one result is saved unasked, and
	// recorded as such.
	if text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/logo.png"})); isErr || !strings.Contains(text, "saved: "+spec.ScratchDir+"/fetch/1-logo.png") {
		t.Fatalf("an over-budget body: (%q, %v)", text, isErr)
	}

	saves := readSaves(t, spec)
	if len(saves) != 3 || saves[0] != (SaveEntry{Path: "assets/logo.png", Host: "127.0.0.1", Bytes: int64(len(png)), SHA256: hex.EncodeToString(sum[:]), Asked: true}) || saves[1].Path != "missing.html" ||
		saves[2] != (SaveEntry{Path: "fetch/1-logo.png", Host: "127.0.0.1", Bytes: int64(len(png)), SHA256: hex.EncodeToString(sum[:])}) {
		t.Fatalf("ledger %+v", saves)
	}
}

// TestFetchSaveToKeepsFetchsRules: saveTo reaches only declared hosts, hop
// by hop; a body holding a run secret is refused even split across the
// copy's parts, and nothing is left; the path stays beneath the scratch
// folder, and a link planted at it is replaced, never written through.
func TestFetchSaveToKeepsFetchsRules(t *testing.T) {
	const secret = "s4ve-to-secret-value-0123456789"
	t.Setenv("SAVE_TEST_SECRET", secret)
	srv, spec, hitOther := saveToFixture(t, secret)
	s := newTestServer(t, spec)
	u, _ := url.Parse(srv.URL)
	fetch := func(args map[string]any) (string, bool) {
		return s.callTool(s.tools[ToolFetch], rawArgs(t, args))
	}

	if text, isErr := fetch(map[string]any{"url": "http://localhost:" + u.Port() + "/notes.txt", "saveTo": "n.txt"}); !isErr || !strings.Contains(text, "not in the egress allowlist") {
		t.Fatalf("an undeclared host: (%q, %v)", text, isErr)
	}
	if text, isErr := fetch(map[string]any{"url": srv.URL + "/away", "saveTo": "away.txt"}); !isErr || !strings.Contains(text, "redirect to host \"localhost\" refused") || hitOther.Load() {
		t.Fatalf("a redirect to an undeclared host: (%q, %v), reached %v", text, isErr, hitOther.Load())
	}
	if text, isErr := fetch(map[string]any{"url": srv.URL + "/secret.bin", "saveTo": "s.bin"}); !isErr || !strings.Contains(text, "secret SAVE_TEST_SECRET, so it was not saved") || strings.Contains(text, secret) {
		t.Fatalf("a body holding a secret: (%q, %v)", text, isErr)
	}
	for _, bad := range []string{"../out.txt", "/etc/passwd", "a/../../b", ".", ""} {
		if bad == "" {
			continue // no saveTo: an ordinary fetch
		}
		if text, isErr := fetch(map[string]any{"url": srv.URL + "/notes.txt", "saveTo": bad}); !isErr || !strings.Contains(text, "saveTo") {
			t.Fatalf("saveTo %q: (%q, %v)", bad, text, isErr)
		}
	}
	if text, isErr := fetch(map[string]any{"url": srv.URL + "/notes.txt", "method": "HEAD", "saveTo": "h.txt"}); !isErr || !strings.Contains(text, "HEAD") {
		t.Fatalf("HEAD with saveTo: (%q, %v)", text, isErr)
	}

	// A declared command may plant a link where the agent will save: a link
	// out of the folder is refused, and its target is left as it was. (One
	// planted after that check is replaced by the save's rename, never
	// written through.)
	outside := filepath.Join(spec.OutDir, "kept.txt")
	mustWrite(t, outside, []byte("kept"))
	if err := os.Symlink(outside, filepath.Join(spec.ScratchDir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if text, isErr := fetch(map[string]any{"url": srv.URL + "/notes.txt", "saveTo": "link.txt"}); !isErr || !strings.Contains(text, "symlink") {
		t.Fatalf("save over a link out: (%q, %v)", text, isErr)
	}
	if b, _ := os.ReadFile(outside); string(b) != "kept" {
		t.Fatalf("the save wrote through a link: %q", b)
	}
	// A link to a file inside the folder is replaced, and its target kept.
	mustWrite(t, filepath.Join(spec.ScratchDir, "inner.txt"), []byte("inner"))
	if err := os.Symlink("inner.txt", filepath.Join(spec.ScratchDir, "near.txt")); err != nil {
		t.Fatal(err)
	}
	if text, isErr := fetch(map[string]any{"url": srv.URL + "/notes.txt", "saveTo": "near.txt"}); isErr {
		t.Fatalf("save over a link within: %q", text)
	}
	if b, _ := os.ReadFile(filepath.Join(spec.ScratchDir, "inner.txt")); string(b) != "inner" {
		t.Fatalf("the save wrote through a link: %q", b)
	}
	if fi, _ := os.Lstat(filepath.Join(spec.ScratchDir, "near.txt")); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the link is still there")
	}

	entries, _ := os.ReadDir(spec.ScratchDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "inner.txt,link.txt,near.txt" {
		t.Fatalf("the scratch folder holds %v after the refusals, want only the planted files and the one save", names)
	}
	if saves := readSaves(t, spec); len(saves) != 1 || saves[0].Path != "near.txt" {
		t.Fatalf("ledger %+v", saves)
	}
}

// TestSaveToNeedsTheScratchFolder: a run without a scratch folder is not
// offered saveTo, and is refused it.
func TestSaveToNeedsTheScratchFolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	s := newTestServer(t, fetchSpecFor(t, srv))
	props := s.tools[ToolFetch].schema["properties"].(map[string]any)
	if _, ok := props["saveTo"]; ok {
		t.Fatal("saveTo offered without a scratch folder")
	}
	if text, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": srv.URL, "saveTo": "x"})); !isErr || !strings.Contains(text, "no scratch folder") {
		t.Fatalf("(%q, %v)", text, isErr)
	}
	spec := fetchSpecFor(t, srv)
	spec.Fetch.Ledger = filepath.Join(spec.OutDir, "fetches.ndjson")
	if err := validateSpec(spec); err == nil {
		t.Fatal("a fetch ledger without a scratch folder validated")
	}
	spec.ScratchDir = t.TempDir()
	spec.Fetch.Ledger = "/elsewhere/fetches.ndjson"
	if err := validateSpec(spec); err == nil {
		t.Fatal("a fetch ledger outside the output folder validated")
	}
}

var base64PageRE = regexp.MustCompile(`^binary file; (\d+) bytes, detected as ([^;]+); bytes (\d+) to (\d+) as base64(?:; pass scratch_read path "([^"]+)", view "original", offset (\d+) for the next page)?:\n([A-Za-z0-9+/=]*)$`)

// TestScratchReadViews: a binary file is read as base64 pages, each within
// the budget, that together are the file; a text file as text; and a
// binary file has no text view.
func TestScratchReadViews(t *testing.T) {
	png := pngFixture()
	for _, lim := range testBudgets {
		b := lim.budget()
		t.Run(b.String(), func(t *testing.T) {
			spec := scratchSpec(t)
			spec.Limits = lim
			mustWrite(t, filepath.Join(spec.ScratchDir, "img", "logo.png"), png)
			mustWrite(t, filepath.Join(spec.ScratchDir, "notes.txt"), []byte("plain text\n"))
			s := newTestServer(t, spec)
			read := func(args map[string]any) (string, bool) {
				return s.callTool(s.tools[ToolScratchRead], rawArgs(t, args))
			}

			var got []byte
			offset, pages := int64(0), 0
			for {
				text, isErr := read(map[string]any{"path": "img/logo.png", "offset": offset})
				if isErr || !b.fits(text) {
					t.Fatalf("offset %d: (%d bytes, %v) %q", offset, len(text), isErr, text[:min(200, len(text))])
				}
				m := base64PageRE.FindStringSubmatch(text)
				if m == nil || m[1] != strconv.Itoa(len(png)) || m[2] != "image/png" || m[3] != strconv.FormatInt(offset, 10) {
					t.Fatalf("offset %d: %q", offset, text[:min(200, len(text))])
				}
				page, err := base64.StdEncoding.DecodeString(m[7])
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, page...)
				pages++
				if m[6] == "" {
					break
				}
				if m[5] != "img/logo.png" {
					t.Fatalf("the notice names %q", m[5])
				}
				offset, _ = strconv.ParseInt(m[6], 10, 64)
			}
			if !bytes.Equal(got, png) || pages < 3 {
				t.Fatalf("%d pages hold %d bytes, not the file's %d", pages, len(got), len(png))
			}
			if text, isErr := read(map[string]any{"path": "img/logo.png", "view": "original"}); isErr || !strings.HasPrefix(text, "binary file; ") {
				t.Fatalf("view original: (%q, %v)", text[:min(100, len(text))], isErr)
			}
			if text, isErr := read(map[string]any{"path": "img/logo.png", "view": "text"}); !isErr || !strings.Contains(text, `read it with view "original"`) {
				t.Fatalf("view text of a binary file: (%q, %v)", text, isErr)
			}
			for _, view := range []string{"", "text", "original"} {
				if text, isErr := read(map[string]any{"path": "notes.txt", "view": view}); isErr || text != "plain text\n" {
					t.Fatalf("a text file in view %q: (%q, %v)", view, text, isErr)
				}
			}
			if text, isErr := read(map[string]any{"path": "notes.txt", "view": "raw"}); !isErr || !strings.Contains(text, "view must be") {
				t.Fatalf("an unknown view: (%q, %v)", text, isErr)
			}
		})
	}
}

// TestScratchSearch: a word in a saved file is found and answered as
// path:line: excerpt; a binary file is not searched; the search stays
// beneath the folder and follows no link out of it.
func TestScratchSearch(t *testing.T) {
	const secret = "s4ve-to-secret-value-0123456789"
	t.Setenv("SAVE_TEST_SECRET", secret)
	srv, spec, _ := saveToFixture(t, secret)
	s := newTestServer(t, spec)
	if text, isErr := s.callTool(s.tools[ToolFetch], rawArgs(t, map[string]any{"url": srv.URL + "/notes.txt", "saveTo": "docs/notes.txt"})); isErr {
		t.Fatal(text)
	}
	mustWrite(t, filepath.Join(spec.ScratchDir, "bin", "needle.png"), append([]byte("\x89PNG\x00needle"), pngFixture()...))
	mustWrite(t, filepath.Join(spec.OutDir, "outside.txt"), []byte("needle outside\n"))
	if err := os.Symlink(spec.OutDir, filepath.Join(spec.ScratchDir, "out")); err != nil {
		t.Fatal(err)
	}
	search := func(args map[string]any) (string, bool) {
		return s.callTool(s.tools[ToolScratchSearch], rawArgs(t, args))
	}

	if text, isErr := search(map[string]any{"query": "NEEDLE"}); isErr || text != "docs/notes.txt:2: the Needle is here" {
		t.Fatalf("(%q, %v)", text, isErr)
	}
	if text, isErr := search(map[string]any{"query": "needle", "path": "docs"}); isErr || text != "docs/notes.txt:2: the Needle is here" {
		t.Fatalf("under a path: (%q, %v)", text, isErr)
	}
	if text, isErr := search(map[string]any{"query": "absent"}); isErr || text != `no scratch text matches "absent"` {
		t.Fatalf("no match: (%q, %v)", text, isErr)
	}
	for _, bad := range []map[string]any{{"query": ""}, {"query": "x", "path": "../"}, {"query": "x", "path": "nowhere"}, {"query": strings.Repeat("q", 300)}} {
		if text, isErr := search(bad); !isErr {
			t.Fatalf("%v: %q", bad, text)
		}
	}
	// More hits than fit are held to the budget, with how many were left out.
	var many strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&many, "needle %d %s\n", i, strings.Repeat("x", 60))
	}
	mustWrite(t, filepath.Join(spec.ScratchDir, "many.txt"), []byte(many.String()))
	text, isErr := search(map[string]any{"query": "needle", "maxResults": 100})
	if isErr || !spec.Limits.budget().fits(text) || !strings.Contains(text, "more matching lines not shown") {
		t.Fatalf("many hits: (%d bytes, %v) %q", len(text), isErr, text[max(0, len(text)-200):])
	}
	if !strings.Contains(spec.Fetch.Ledger, "receipts") {
		t.Fatal("fixture")
	}
}

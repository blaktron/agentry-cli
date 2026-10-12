package source

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func urlOptions(t *testing.T, srv *httptest.Server, p string) Options {
	return Options{Kind: KindURL, URL: srv.URL + p, Out: t.TempDir(), Work: t.TempDir(), Transport: srv.Client().Transport, anyPort: true}
}

// A url source is one GET, saved as one file named by the host's
// Content-Disposition, with the bearer token sent and no transfer encoding
// asked for.
func TestURLFetchSavesOneFile(t *testing.T) {
	var gotAuth, gotEnc string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotEnc = r.Header.Get("Authorization"), r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Disposition", `attachment; filename="Q3 report.pdf"`)
		_, _ = w.Write([]byte("%PDF-1.4 body"))
	}))
	defer srv.Close()
	o := urlOptions(t, srv, "/files/get?id=7")
	o.Token = "tok_plantedURLtoken0123"
	rep, err := Fetch(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Kind != KindURL || rep.Files != 1 || rep.Bytes != 13 || rep.Name != "Q3 report.pdf" {
		t.Errorf("report %+v", rep)
	}
	if got := listFiles(t, o.Out); len(got) != 1 || got[0] != "Q3 report.pdf" {
		t.Errorf("staged %v", got)
	}
	if gotAuth != "Bearer "+o.Token || gotEnc != "identity" {
		t.Errorf("Authorization %q, Accept-Encoding %q", gotAuth, gotEnc)
	}
}

func TestURLFetchNamesTheFileFromTheAddress(t *testing.T) {
	for p, want := range map[string]string{
		"/docs/annual%20report.pdf": "annual report.pdf",
		"/":                         fallbackName,
		"/a/CON.txt":                "_CON.txt",
		"/a/..":                     fallbackName,
		"/a/evil%E2%80%AEtxt.exe":   "evil_txt.exe",
		"/a/" + strings.Repeat("x", 300) + ".pdf": strings.Repeat("x", maxFileName-4) + ".pdf",
	} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
		o := urlOptions(t, srv, p)
		rep, err := Fetch(context.Background(), o)
		srv.Close()
		if err != nil || rep.Name != want {
			t.Errorf("%s: name %q, want %q (%v)", p, rep.Name, want, err)
		}
	}
}

func TestPortableFileName(t *testing.T) {
	for raw, want := range map[string]string{
		"../../etc/passwd": "passwd",
		`C:\Windows\a.dll`: "a.dll",
		"a:b*c?.txt":       "a_b_c_.txt",
		"name. . ":         "name",
		"  lead.txt":       "lead.txt",
		".git":             "",
		"GIT~1":            "",
		"nul":              "_nul",
		"COM1.log":         "_COM1.log",
		"zero\u200bwidth":  "zero_width",
		"tab\tname":        "tab_name",
		"":                 "",
		"résumé.pdf":       "résumé.pdf",
		"bad\xffutf8.txt":  "bad_utf8.txt",
	} {
		if got := PortableFileName(raw); got != want {
			t.Errorf("PortableFileName(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A body past the cap is refused, by its declared length or as it streams,
// and nothing is left under Out.
func TestURLFetchOverTheCapStagesNothing(t *testing.T) {
	body := strings.Repeat("a", 2048)
	for name, declare := range map[string]bool{"declared": true, "streamed": false} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if declare {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				} else {
					w.Header().Set("Transfer-Encoding", "chunked")
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			o := urlOptions(t, srv, "/big.bin")
			o.MaxBytes = 1024
			rep, err := Fetch(context.Background(), o)
			if err != nil || !strings.Contains(rep.Refusal, "1.0 KiB cap") {
				t.Fatalf("refusal %q, err %v", rep.Refusal, err)
			}
			// Typed (agentry-cli#884): the declared length, or the cap
			// the stream ran past.
			want := Over{Cap: CapBytes, Of: OfBytes, Count: int64(len(body))}
			if !declare {
				want = Over{Cap: CapBytes, Of: OfBytes, Count: 1024, More: true}
			}
			if rep.Over == nil || *rep.Over != want {
				t.Errorf("over %+v, want %+v", rep.Over, want)
			}
			if got := listFiles(t, o.Out); len(got) != 0 {
				t.Errorf("staged in part: %v", got)
			}
		})
	}
}

func TestURLFetchSaysWhyAHostRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		http.Error(w, "nope "+r.Header.Get("Authorization"), http.StatusForbidden)
	}))
	defer srv.Close()
	o := urlOptions(t, srv, "/f.pdf")
	rep, err := Fetch(context.Background(), o)
	if err == nil || !strings.Contains(rep.Error, "404 Not Found: the file does not exist, or it is private") {
		t.Errorf("anonymous: %v / %q", err, rep.Error)
	}
	o.Work = t.TempDir()
	o.Token = "tok_plantedURLtoken0123"
	rep, err = Fetch(context.Background(), o)
	if err == nil || !strings.Contains(rep.Error, "refused the access token given for this file") {
		t.Errorf("with a token: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(o.Work, ReportFile))
	if strings.Contains(string(raw), o.Token) {
		t.Error("the token reached the report")
	}
}

// A redirect on the same host is followed with the token; one to another
// host is refused before the token is sent there.
func TestURLFetchFollowsRedirectsOnTheHostAlone(t *testing.T) {
	var sawAuth bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = sawAuth || r.Header.Get("Authorization") != ""
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/here":
			http.Redirect(w, r, "/there/file.txt", http.StatusFound)
		case "/there/file.txt":
			if r.Header.Get("Authorization") == "" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("ok"))
		default:
			http.Redirect(w, r, other.URL+"/x", http.StatusFound)
		}
	}))
	defer srv.Close()
	o := urlOptions(t, srv, "/here")
	o.Token = "t0ken"
	rep, err := Fetch(context.Background(), o)
	if err != nil || rep.Name != "file.txt" {
		t.Errorf("same host: %+v %v", rep, err)
	}
	o = urlOptions(t, srv, "/away")
	o.Token = "t0ken"
	if _, err := Fetch(context.Background(), o); err == nil || !strings.Contains(err.Error(), "redirected to another address") {
		t.Errorf("another host: %v", err)
	}
	if sawAuth {
		t.Error("the token followed the redirect")
	}
}

func TestCheckFileURL(t *testing.T) {
	for _, bad := range []string{
		"http://example.com/f.pdf", "ftp://example.com/f.pdf", "file:///etc/passwd",
		"https://u:p@example.com/f.pdf", "https://example.com:8443/f.pdf", "https://example.com/f.pdf#page=2",
		"https://exa_mple.com/f.pdf", "https://example.com,evil.example/f", "https://10.0.0.1:8443/f",
	} {
		if _, err := CheckFileURL(bad); err == nil {
			t.Errorf("CheckFileURL(%q) passed", bad)
		}
	}
	for _, good := range []string{"https://example.com/f.pdf", "https://example.com/get?id=7&sig=abc", "https://example.com", "https://example.com:443/f"} {
		if _, err := CheckFileURL(good); err != nil {
			t.Errorf("CheckFileURL(%q): %v", good, err)
		}
	}
}

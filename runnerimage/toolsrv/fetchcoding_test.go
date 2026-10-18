package toolsrv

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// encodeAs is body in the named content coding.
func encodeAs(t *testing.T, coding string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w interface {
		Write([]byte) (int, error)
		Close() error
	}
	switch coding {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "deflate":
		w = zlib.NewWriter(&buf)
	case "deflate-raw":
		fw, err := flate.NewWriter(&buf, flate.BestCompression)
		if err != nil {
			t.Fatal(err)
		}
		w = fw
	case "br":
		return brotliOf(t, body)
	default:
		t.Fatalf("no encoder for %q", coding)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// codingFixture is a host that serves body (set by each test) in the
// coding the query names (/body?coding=), under the Content-Encoding lines
// in headers when any are set, and unencoded at /plain.
type codingFixture struct {
	srv     *httptest.Server
	body    []byte
	headers []string
}

func newCodingFixture(t *testing.T) *codingFixture {
	t.Helper()
	f := &codingFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch r.URL.Path {
		case "/plain":
			_, _ = w.Write(f.body)
		case "/xgzip":
			w.Header().Set("Content-Encoding", "  X-GZip ")
			_, _ = w.Write(encodeAs(t, "gzip", f.body))
		case "/body":
			coding := r.URL.Query().Get("coding")
			enc := coding
			switch coding {
			case "deflate-raw":
				enc = "deflate"
			case "none":
				enc = ""
			}
			if len(f.headers) > 0 {
				for _, h := range f.headers {
					w.Header().Add("Content-Encoding", h)
				}
			} else if enc != "" {
				w.Header().Set("Content-Encoding", enc)
			}
			if r.Method == http.MethodHead {
				return
			}
			switch coding {
			case "", "none":
				_, _ = w.Write(f.body)
			case "corrupt":
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = w.Write([]byte("this is not gzip at all"))
			case "empty":
				w.Header().Set("Content-Encoding", "gzip")
			default:
				_, _ = w.Write(encodeAs(t, coding, f.body))
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func codingServer(t *testing.T, f *codingFixture, secret string) *server {
	t.Helper()
	spec := fetchSpecFor(t, f.srv, "CODING_TEST_SECRET")
	spec.ScratchDir = t.TempDir()
	spec.Fetch.Ledger = filepath.Join(spec.OutDir, "receipts", "fetches.ndjson")
	// A small reply cap makes the decoded-size limit small too.
	spec.Limits.MaxFetchBytes = 4 << 10
	t.Setenv("CODING_TEST_SECRET", secret)
	return newTestServer(t, spec)
}

func fetchWith(t *testing.T, s *server, args map[string]any) (string, bool) {
	t.Helper()
	return s.callTool(s.tools[ToolFetch], rawArgs(t, args))
}

func scratchNames(t *testing.T, s *server) []string {
	t.Helper()
	var names []string
	_ = filepath.WalkDir(s.spec.ScratchDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(s.spec.ScratchDir, p)
			names = append(names, rel)
		}
		return nil
	})
	return names
}

// TestFetchDecodesBeforeItScrubs: a body the host sends in gzip, deflate
// (zlib-wrapped or raw) or br, at the caller's request, has a granted
// secret's value found: scrubbed from the answer, and refused by the
// over-budget save and by saveTo.
func TestFetchDecodesBeforeItScrubs(t *testing.T) {
	const secret = "enc0ded-secret-value-0123456789abcdef"
	for _, coding := range []string{"gzip", "deflate", "deflate-raw", "br"} {
		t.Run(coding, func(t *testing.T) {
			f := newCodingFixture(t)
			s := codingServer(t, f, secret)
			args := func(extra map[string]any) map[string]any {
				m := map[string]any{
					"url":     f.srv.URL + "/body?coding=" + coding,
					"headers": map[string]string{"Accept-Encoding": "gzip, deflate, br"},
				}
				for k, v := range extra {
					m[k] = v
				}
				return m
			}

			// The answer: scrubbed, and plain text, not compressed bytes.
			f.body = []byte("before " + secret + " after")
			text, isErr := fetchWith(t, s, args(nil))
			if isErr || strings.Contains(text, secret) || !strings.Contains(text, "before [redacted:CODING_TEST_SECRET] after") {
				t.Fatalf("an encoded answer holding a secret: (%q, %v)", text, isErr)
			}

			// An over-budget body is not saved when it holds the value.
			f.body = append([]byte(secret+"\n"), bytes.Repeat([]byte("filler line\n"), 600)...)
			text, isErr = fetchWith(t, s, args(nil))
			if !isErr || !strings.Contains(text, "holds the value of this run's secret CODING_TEST_SECRET, so it was not saved") || strings.Contains(text, secret) {
				t.Fatalf("an encoded over-budget body holding a secret: (%q, %v)", text, isErr)
			}

			// saveTo refuses it and keeps nothing.
			text, isErr = fetchWith(t, s, args(map[string]any{"saveTo": "out.txt"}))
			if !isErr || !strings.Contains(text, "secret CODING_TEST_SECRET, so it was not saved") || strings.Contains(text, secret) {
				t.Fatalf("saveTo of an encoded body holding a secret: (%q, %v)", text, isErr)
			}
			if names := scratchNames(t, s); len(names) != 0 {
				t.Fatalf("the scratch folder holds %v after the refusals", names)
			}
			if saves := readSaves(t, s.spec); len(saves) != 0 {
				t.Fatalf("ledger %+v after the refusals", saves)
			}

			// The same coding without the value: saveTo keeps the decoded
			// bytes, and the ledger names their size and hash.
			clean := append(bytes.Repeat([]byte("a clean line\n"), 800), 0, 1, 2)
			f.body = clean
			text, isErr = fetchWith(t, s, args(map[string]any{"saveTo": "out.bin"}))
			sum := sha256.Sum256(clean)
			if isErr || !strings.Contains(text, hex.EncodeToString(sum[:])) || !strings.Contains(text, "content-encoding: "+strings.TrimSuffix(coding, "-raw")+" (decoded") {
				t.Fatalf("saveTo of a clean encoded body: (%q, %v)", text, isErr)
			}
			if strings.Contains(text, "content-length") {
				t.Errorf("the answer reports the encoded length as the content length: %q", text)
			}
			got, err := os.ReadFile(filepath.Join(s.spec.ScratchDir, "out.bin"))
			if err != nil || !bytes.Equal(got, clean) {
				t.Fatalf("the saved file is not the decoded body (%d bytes, %v)", len(got), err)
			}
			if saves := readSaves(t, s.spec); len(saves) != 1 || saves[0].Bytes != int64(len(clean)) || saves[0].SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("ledger %+v, want the decoded size and hash", saves)
			}
		})
	}
}

// TestFetchSaveToWatchSeesTheDecodedBytes: a value that exists only in the
// decoded stream, split across the copy's parts, stops the save.
func TestFetchSaveToWatchSeesTheDecodedBytes(t *testing.T) {
	const secret = "split-enc0ded-secret-abcdef-0123"
	f := newCodingFixture(t)
	s := codingServer(t, f, secret)
	// Random-looking filler so the value straddles a part boundary in the
	// decoded stream whatever the compressor does with it.
	body := bytes.Repeat([]byte{'x'}, 32<<10-5)
	f.body = append(append(body, secret...), 'y', 'z')
	text, isErr := fetchWith(t, s, map[string]any{
		"url":     f.srv.URL + "/body?coding=gzip",
		"headers": map[string]string{"Accept-Encoding": "gzip"},
		"saveTo":  "x.txt",
	})
	if !isErr || !strings.Contains(text, "so it was not saved") {
		t.Fatalf("(%q, %v)", text, isErr)
	}
	if names := scratchNames(t, s); len(names) != 0 {
		t.Fatalf("the scratch folder holds %v", names)
	}
}

// TestFetchRefusesADecompressionBomb: a body that decodes past the limit
// is refused by saveTo, and nothing is kept; an answer reads at most the
// reply cap of it.
func TestFetchRefusesADecompressionBomb(t *testing.T) {
	for _, coding := range []string{"gzip", "deflate", "br"} {
		t.Run(coding, func(t *testing.T) {
			f := newCodingFixture(t)
			s := codingServer(t, f, "unused-secret-value-xyz")
			limit := s.spec.Limits.decodedFetchCap()
			if limit != 64*s.spec.Limits.fetchCap() {
				t.Fatalf("the decoded limit is %d, want 64 times the reply cap", limit)
			}
			f.body = make([]byte, limit+1<<20) // zeros: tiny encoded
			args := map[string]any{
				"url":     f.srv.URL + "/body?coding=" + coding,
				"headers": map[string]string{"Accept-Encoding": coding},
			}
			args["saveTo"] = "bomb.bin"
			text, isErr := fetchWith(t, s, args)
			if !isErr || !strings.Contains(text, "decodes to more than") {
				t.Fatalf("a bomb saved with saveTo: (%q, %v)", text, isErr)
			}
			if names := scratchNames(t, s); len(names) != 0 {
				t.Fatalf("the scratch folder holds %v after a refused bomb", names)
			}
			if saves := readSaves(t, s.spec); len(saves) != 0 {
				t.Fatalf("ledger %+v after a refused bomb", saves)
			}

			// Without saveTo the answer reads the reply cap and no more,
			// and says the rest was not read.
			delete(args, "saveTo")
			text, isErr = fetchWith(t, s, args)
			if isErr || !strings.Contains(text, "fetch reads no more of a body") {
				t.Fatalf("a bomb answered: (%.200q, %v)", text, isErr)
			}
			if !strings.Contains(text, "bytes of more") {
				t.Errorf("the truncation note does not say the decoded length is unknown: %.300q", text)
			}
		})
	}
}

// TestFetchDecodesWhatTheRequestDidNotAskFor: a host that encodes a body
// although the request set no Accept-Encoding is decoded, and held to the
// same limit; one that stacks gzip twice is refused, not half decoded.
func TestFetchDecodesWhatTheRequestDidNotAskFor(t *testing.T) {
	const secret = "unasked-secret-value-0123456789"
	f := newCodingFixture(t)
	s := codingServer(t, f, secret)
	bomb := encodeAs(t, "gzip", make([]byte, s.spec.Limits.decodedFetchCap()+1<<20))
	f.headers = []string{"gzip"}
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("fetch asked for an encoding the caller did not: %q", r.Header.Get("Accept-Encoding"))
		}
		switch r.URL.Path {
		case "/bomb":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(bomb)
		case "/twice":
			w.Header().Add("Content-Encoding", "gzip")
			w.Header().Add("Content-Encoding", "gzip")
			_, _ = w.Write(encodeAs(t, "gzip", encodeAs(t, "gzip", []byte("hidden "+secret))))
		}
	})
	text, isErr := fetchWith(t, s, map[string]any{"url": f.srv.URL + "/bomb", "saveTo": "auto.bin"})
	if !isErr || !strings.Contains(text, "decodes to more than") {
		t.Fatalf("(%q, %v)", text, isErr)
	}
	text, isErr = fetchWith(t, s, map[string]any{"url": f.srv.URL + "/twice"})
	if !isErr || !strings.Contains(text, "more than one content coding") {
		t.Fatalf("gzip twice: (%q, %v)", text, isErr)
	}
	if names := scratchNames(t, s); len(names) != 0 {
		t.Fatalf("the scratch folder holds %v", names)
	}
}

// TestFetchRefusesAStackedCoding: more than one coding, in one header
// value or several lines, is refused before any of it is read.
func TestFetchRefusesAStackedCoding(t *testing.T) {
	for name, headers := range map[string][]string{
		"one value":             {"gzip, br"},
		"two lines":             {"gzip", "br"},
		"the same twice":        {"gzip, gzip"},
		"with identity and two": {"identity, gzip, gzip"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCodingFixture(t)
			s := codingServer(t, f, "unused-secret-value-xyz")
			f.body = []byte("hello")
			f.headers = headers
			for _, extra := range []map[string]any{nil, {"saveTo": "s.txt"}} {
				args := map[string]any{"url": f.srv.URL + "/body?coding=gzip"}
				for k, v := range extra {
					args[k] = v
				}
				text, isErr := fetchWith(t, s, args)
				if !isErr || !strings.Contains(text, "more than one content coding") {
					t.Fatalf("%v: (%q, %v)", extra, text, isErr)
				}
			}
			if names := scratchNames(t, s); len(names) != 0 {
				t.Fatalf("the scratch folder holds %v", names)
			}
		})
	}
}

// TestFetchRefusesACodingItCannotDecode: an unknown coding is refused, not
// passed through as bytes.
func TestFetchRefusesACodingItCannotDecode(t *testing.T) {
	for _, coding := range []string{"zstd", "compress", "x-unheard-of", "GZIP2"} {
		t.Run(coding, func(t *testing.T) {
			f := newCodingFixture(t)
			s := codingServer(t, f, "unused-secret-value-xyz")
			f.body = []byte("hello")
			f.headers = []string{coding}
			for _, extra := range []map[string]any{nil, {"saveTo": "s.txt"}} {
				args := map[string]any{"url": f.srv.URL + "/body?coding=none"}
				for k, v := range extra {
					args[k] = v
				}
				text, isErr := fetchWith(t, s, args)
				if !isErr || !strings.Contains(text, "cannot decode") || strings.Contains(text, "hello") {
					t.Fatalf("%v: (%q, %v)", extra, text, isErr)
				}
			}
			if names := scratchNames(t, s); len(names) != 0 {
				t.Fatalf("the scratch folder holds %v", names)
			}
		})
	}
}

// TestFetchIdentityIsUnchanged: a body with no coding, or the identity
// one, is served and saved as it is, and a HEAD, which has no body, is not
// held to its Content-Encoding.
func TestFetchIdentityIsUnchanged(t *testing.T) {
	f := newCodingFixture(t)
	s := codingServer(t, f, "unused-secret-value-xyz")
	f.body = append(bytes.Repeat([]byte("plain line\n"), 700), 0, 255)
	for _, header := range [][]string{nil, {"identity"}, {" Identity "}} {
		f.headers = header
		text, isErr := fetchWith(t, s, map[string]any{"url": f.srv.URL + "/body?coding=none", "saveTo": "p.bin"})
		sum := sha256.Sum256(f.body)
		if isErr || !strings.Contains(text, hex.EncodeToString(sum[:])) || strings.Contains(text, "decoded") {
			t.Fatalf("%v: (%q, %v)", header, text, isErr)
		}
		if got, _ := os.ReadFile(filepath.Join(s.spec.ScratchDir, "p.bin")); !bytes.Equal(got, f.body) {
			t.Fatalf("%v: the saved file differs from the body", header)
		}
	}
	f.headers = nil
	f.body = []byte("short plain answer")
	if text, isErr := fetchWith(t, s, map[string]any{"url": f.srv.URL + "/plain"}); isErr || !strings.HasSuffix(text, "short plain answer") {
		t.Fatalf("a plain answer: (%q, %v)", text, isErr)
	}
	f.headers = []string{"zstd"}
	if text, isErr := fetchWith(t, s, map[string]any{"url": f.srv.URL + "/body?coding=none", "method": "HEAD"}); isErr {
		t.Fatalf("a HEAD naming a coding it never sends a body in: %q", text)
	}
}

// TestFetchEncodedEdges: an empty encoded body is an empty answer, a body
// that is not in the coding it names is an error, and a coding name is
// matched without regard to case and surrounding space.
func TestFetchEncodedEdges(t *testing.T) {
	f := newCodingFixture(t)
	s := codingServer(t, f, "unused-secret-value-xyz")
	if text, isErr := fetchWith(t, s, map[string]any{"url": f.srv.URL + "/body?coding=empty", "headers": map[string]string{"Accept-Encoding": "gzip"}}); isErr || !strings.HasSuffix(text, "\n\n") {
		t.Fatalf("an empty gzip body: (%q, %v)", text, isErr)
	}
	if text, isErr := fetchWith(t, s, map[string]any{"url": f.srv.URL + "/body?coding=corrupt", "headers": map[string]string{"Accept-Encoding": "gzip"}}); !isErr || !strings.Contains(text, "read response") {
		t.Fatalf("a body that is not gzip: (%q, %v)", text, isErr)
	}
	if text, isErr := fetchWith(t, s, map[string]any{"url": f.srv.URL + "/body?coding=corrupt", "headers": map[string]string{"Accept-Encoding": "gzip"}, "saveTo": "c.txt"}); !isErr || !strings.Contains(text, "read response") {
		t.Fatalf("a body that is not gzip, saved: (%q, %v)", text, isErr)
	}
	if names := scratchNames(t, s); len(names) != 0 {
		t.Fatalf("the scratch folder holds %v", names)
	}
	f.body = []byte("odd case")
	if text, isErr := fetchWith(t, s, map[string]any{"url": f.srv.URL + "/xgzip", "headers": map[string]string{"Accept-Encoding": "gzip"}}); isErr || !strings.HasSuffix(text, "odd case") {
		t.Fatalf("a coding named x-gzip with stray case and space: (%q, %v)", text, isErr)
	}
}

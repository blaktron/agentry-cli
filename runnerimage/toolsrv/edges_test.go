// Edge-branch tests the per-tool suites do not reach: argument-decode
// refusals on every tool, filesystem error surfaces (unreadable files,
// paths blocked by files), the fetch redirect gates hop by hop, exec shim
// construction failures, and the MCP loop's own failure lines. Each one is
// a branch a regression could silently widen, so each is pinned.

package toolsrv

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolArgumentDecodeRefusals(t *testing.T) {
	spec := inputsSpec(t)
	spec.FSRead = Access{Enabled: true}
	spec.FSWrite = Access{Enabled: true}
	spec.Fetch = Fetch{Enabled: true, Hosts: []string{"api.example.com"}}
	spec.Exec = Exec{Commands: []string{"echo"}}
	s := newTestServer(t, spec)

	garbage := json.RawMessage(`{"path":`)
	calls := map[string]func(json.RawMessage) (string, bool){
		"fs_read":      s.toolFSRead,
		"fs_list":      s.toolFSList,
		"fs_write":     s.toolFSWrite,
		"fetch":        s.toolFetch,
		"run_command":  s.toolRunCommand,
		"write_report": s.toolWriteReport,
	}
	for name, call := range calls {
		got, isErr := call(garbage)
		if !isErr || !strings.Contains(got, "invalid arguments") {
			t.Errorf("%s(garbage) = (%q, %v)", name, got, isErr)
		}
	}

	// Missing required fields are named, not guessed.
	if got, isErr := s.toolFSWrite(rawArgs(t, map[string]any{"content": "x"})); !isErr || !strings.Contains(got, "path is required") {
		t.Errorf("fs_write no path = (%q, %v)", got, isErr)
	}
	if got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{})); !isErr || !strings.Contains(got, "command is required") {
		t.Errorf("run_command no command = (%q, %v)", got, isErr)
	}
}

func TestTrimPartialRune(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"valid stays whole", []byte("hello"), []byte("hello")},
		{"split rune shaved", []byte{'h', 0xC3}, []byte("h")}, // "hé" cut mid-é
		{"binary loses at most three bytes", []byte{0xff, 0xff, 0xff, 0xff, 0xff}, []byte{0xff, 0xff}},
		{"empty stays empty", []byte{}, []byte{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := trimPartialRune(append([]byte(nil), tc.in...))
			if string(got) != string(tc.want) {
				t.Errorf("trimPartialRune(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFSReadUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not refuse reads")
	}
	spec := baseSpec(t)
	spec.FSRead = Access{Enabled: true}
	locked := filepath.Join(spec.WorkspaceDir, "locked.txt")
	mustWrite(t, locked, []byte("secret"))
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	s := newTestServer(t, spec)
	got, isErr := s.toolFSRead(rawArgs(t, map[string]any{"path": "locked.txt"}))
	if !isErr || !strings.Contains(got, "fs_read:") {
		t.Errorf("(%q, %v)", got, isErr)
	}
	// Same surface on the inputs twin.
	ispec := inputsSpec(t)
	ilocked := filepath.Join(ispec.InputsDir, "locked.txt")
	mustWrite(t, ilocked, []byte("secret"))
	if err := os.Chmod(ilocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ilocked, 0o644) })
	is := newTestServer(t, ispec)
	got, isErr = is.toolReadInput(rawArgs(t, map[string]any{"path": "locked.txt"}))
	if !isErr || !strings.Contains(got, "read_input:") {
		t.Errorf("(%q, %v)", got, isErr)
	}
}

func TestFSWriteTargetBlocked(t *testing.T) {
	spec := baseSpec(t)
	spec.FSWrite = Access{Enabled: true}
	mustWrite(t, filepath.Join(spec.WorkspaceDir, "occupied.txt"), []byte("x"))
	if err := os.MkdirAll(filepath.Join(spec.WorkspaceDir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, spec)

	// Parent "directory" is an existing file: MkdirAll must refuse.
	got, isErr := s.toolFSWrite(rawArgs(t, map[string]any{"path": "occupied.txt/nested.txt", "content": "x"}))
	if !isErr || !strings.Contains(got, "fs_write:") {
		t.Errorf("file-as-dir = (%q, %v)", got, isErr)
	}
	// Target is an existing directory: WriteFile must refuse.
	got, isErr = s.toolFSWrite(rawArgs(t, map[string]any{"path": "adir", "content": "x"}))
	if !isErr || !strings.Contains(got, "fs_write:") {
		t.Errorf("dir-as-file = (%q, %v)", got, isErr)
	}
}

func TestWriteReportErrorSurfaces(t *testing.T) {
	t.Run("reports root blocked by a file", func(t *testing.T) {
		spec := baseSpec(t)
		mustWrite(t, filepath.Join(spec.OutDir, "reports"), []byte("not a dir"))
		s := newTestServer(t, spec)
		got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "r.md", "content": "x"}))
		if !isErr || !strings.Contains(got, "write_report:") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("nested parent blocked by a file", func(t *testing.T) {
		spec := baseSpec(t)
		s := newTestServer(t, spec)
		if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "plain.md", "content": "x"})); isErr {
			t.Fatalf("seed write failed: %q", got)
		}
		got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "plain.md/sub.md", "content": "x"}))
		if !isErr || !strings.Contains(got, "write_report:") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("append and overwrite refuse a directory target", func(t *testing.T) {
		spec := baseSpec(t)
		if err := os.MkdirAll(filepath.Join(spec.OutDir, "reports", "adir"), 0o755); err != nil {
			t.Fatal(err)
		}
		s := newTestServer(t, spec)
		got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "adir", "content": "x", "append": true}))
		if !isErr || !strings.Contains(got, "write_report:") {
			t.Errorf("append onto dir = (%q, %v)", got, isErr)
		}
		got, isErr = s.toolWriteReport(rawArgs(t, map[string]any{"path": "adir", "content": "x"}))
		if !isErr || !strings.Contains(got, "write_report:") {
			t.Errorf("overwrite onto dir = (%q, %v)", got, isErr)
		}
	})
}

// fetchSpecFor allows the test server's own loopback host.
func fetchSpecFor(t *testing.T, srv *httptest.Server, secrets ...string) *Spec {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	spec := baseSpec(t)
	spec.Fetch = Fetch{Enabled: true, Hosts: []string{u.Hostname()}, SecretEnv: secrets}
	return spec
}

func TestFetchEdges(t *testing.T) {
	var handler func(w http.ResponseWriter, r *http.Request)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler(w, r) }))
	t.Cleanup(srv.Close)

	t.Run("unparseable url", func(t *testing.T) {
		s := newTestServer(t, fetchSpecFor(t, srv))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": "http://bad host/"}))
		if !isErr || !strings.Contains(got, "invalid url") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("auth requires env name", func(t *testing.T) {
		s := newTestServer(t, fetchSpecFor(t, srv, "API_TOKEN"))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": srv.URL, "auth": map[string]any{}}))
		if !isErr || !strings.Contains(got, "auth.env is required") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("auth secret unset", func(t *testing.T) {
		t.Setenv("FETCH_EDGE_TOKEN", "")
		s := newTestServer(t, fetchSpecFor(t, srv, "FETCH_EDGE_TOKEN"))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": srv.URL, "auth": map[string]any{"env": "FETCH_EDGE_TOKEN"}}))
		if !isErr || !strings.Contains(got, "not set") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("auth header scheme sets the named header", func(t *testing.T) {
		t.Setenv("FETCH_EDGE_TOKEN", "tok-abc")
		var sawHeader string
		handler = func(w http.ResponseWriter, r *http.Request) {
			sawHeader = r.Header.Get("X-Api-Key")
			_, _ = w.Write([]byte("ok"))
		}
		s := newTestServer(t, fetchSpecFor(t, srv, "FETCH_EDGE_TOKEN"))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{
			"url":  srv.URL,
			"auth": map[string]any{"env": "FETCH_EDGE_TOKEN", "scheme": "header", "header": "X-Api-Key"},
		}))
		if isErr {
			t.Fatalf("(%q, %v)", got, isErr)
		}
		if sawHeader != "tok-abc" {
			t.Errorf("header value = %q", sawHeader)
		}
		if strings.Contains(got, "tok-abc") {
			t.Error("secret value surfaced in tool output")
		}
	})
	t.Run("auth header scheme requires a header name", func(t *testing.T) {
		t.Setenv("FETCH_EDGE_TOKEN", "tok-abc")
		s := newTestServer(t, fetchSpecFor(t, srv, "FETCH_EDGE_TOKEN"))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{
			"url":  srv.URL,
			"auth": map[string]any{"env": "FETCH_EDGE_TOKEN", "scheme": "header"},
		}))
		if !isErr || !strings.Contains(got, "auth.header is required") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("allowed same-host redirect is followed", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/hop" {
				_, _ = w.Write([]byte("landed"))
				return
			}
			http.Redirect(w, r, "/hop", http.StatusFound)
		}
		s := newTestServer(t, fetchSpecFor(t, srv))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": srv.URL + "/start"}))
		if isErr || !strings.Contains(got, "landed") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("redirect loop stops at ten hops", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/again", http.StatusFound)
		}
		s := newTestServer(t, fetchSpecFor(t, srv))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": srv.URL}))
		if !isErr || !strings.Contains(got, "stopped after 10 redirects") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("redirect to a foreign scheme refused", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "ftp://files.example.com/x")
			w.WriteHeader(http.StatusFound)
		}
		s := newTestServer(t, fetchSpecFor(t, srv))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": srv.URL}))
		if !isErr || !strings.Contains(got, `scheme "ftp" refused`) {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("response body dying mid-read is an error", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("short"))
			// Handler returns early: the connection closes before the
			// promised bytes arrive and the client read fails.
		}
		s := newTestServer(t, fetchSpecFor(t, srv))
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": srv.URL}))
		if !isErr || !strings.Contains(got, "read response") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("truncation note without a content length", func(t *testing.T) {
		handler = func(w http.ResponseWriter, r *http.Request) {
			fl, _ := w.(http.Flusher)
			for i := 0; i < 64; i++ {
				_, _ = w.Write([]byte(strings.Repeat("x", 100)))
				if fl != nil {
					fl.Flush() // chunked: ContentLength stays unknown
				}
			}
		}
		spec := fetchSpecFor(t, srv)
		spec.Limits.MaxFetchBytes = 50
		s := newTestServer(t, spec)
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": srv.URL}))
		if isErr || !strings.Contains(got, "[truncated at 50 bytes]") || strings.Contains(got, "bytes of") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
}

func TestBuildShimEdges(t *testing.T) {
	t.Run("path-bearing declared command gets no shim", func(t *testing.T) {
		spec := baseSpec(t)
		spec.Exec = Exec{Commands: []string{"/bin/echo"}}
		var logs strings.Builder
		s, err := newServer(spec, &strings.Builder{}, &logs)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.shims) != 0 {
			t.Errorf("shims = %v, want none", s.shims)
		}
		if !strings.Contains(logs.String(), "not a bare name") {
			t.Errorf("logs = %q", logs.String())
		}
		// The call-time gate refuses it independently.
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "/bin/echo"}))
		if !isErr || !strings.Contains(got, "path separators are refused") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("duplicate declared command fails shim construction", func(t *testing.T) {
		spec := baseSpec(t)
		spec.Exec = Exec{Commands: []string{"echo", "echo"}}
		if _, err := newServer(spec, &strings.Builder{}, nil); err == nil || !strings.Contains(err.Error(), "shim echo") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("shim vanishing between start and call", func(t *testing.T) {
		spec := baseSpec(t)
		spec.Exec = Exec{Commands: []string{"echo"}}
		s := newTestServer(t, spec)
		if err := os.Remove(s.shims["echo"]); err != nil {
			t.Fatal(err)
		}
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "echo"}))
		if !isErr || !strings.Contains(got, "run_command:") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
	t.Run("caller timeout tighter than the server cap", func(t *testing.T) {
		spec := baseSpec(t)
		spec.Exec = Exec{Commands: []string{"sleep"}}
		s := newTestServer(t, spec)
		if _, ok := s.shims["sleep"]; !ok {
			t.Skip("sleep not present in the test image")
		}
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{
			"command": "sleep", "args": []string{"5"}, "timeoutSeconds": 1,
		}))
		if !isErr || !strings.Contains(got, "timed out after 1s") {
			t.Errorf("(%q, %v)", got, isErr)
		}
	})
}

func TestResolveUnderErrorSurfaces(t *testing.T) {
	t.Run("missing root refuses everything", func(t *testing.T) {
		if _, err := resolveUnder(filepath.Join(t.TempDir(), "gone"), "x"); err == nil || !strings.Contains(err.Error(), "resolve tool root") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dangling symlink refused", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "dangling")); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveUnder(root, "dangling"); err == nil || !strings.Contains(err.Error(), "resolve") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestServeLoopFailureLines(t *testing.T) {
	t.Run("wrong spec version refused before serving", func(t *testing.T) {
		spec := baseSpec(t)
		spec.SpecVersion = "1"
		err := Serve(strings.NewReader(""), &strings.Builder{}, nil, spec)
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("relative inputs dir refused", func(t *testing.T) {
		spec := baseSpec(t)
		spec.InputsDir = "relative/inputs"
		err := Serve(strings.NewReader(""), &strings.Builder{}, nil, spec)
		if err == nil || !strings.Contains(err.Error(), "inputsDir") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("shim failure aborts serve", func(t *testing.T) {
		spec := baseSpec(t)
		spec.Exec = Exec{Commands: []string{"echo", "echo"}}
		if err := Serve(strings.NewReader(""), &strings.Builder{}, nil, spec); err == nil {
			t.Fatal("duplicate shim must abort Serve")
		}
	})
	t.Run("oversized line kills the stream read", func(t *testing.T) {
		huge := strings.Repeat("x", maxLineBytes+1)
		err := Serve(strings.NewReader(huge), &strings.Builder{}, nil, baseSpec(t))
		if err == nil || !strings.Contains(err.Error(), "read mcp stream") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("id without method gets an invalid-request error", func(t *testing.T) {
		resps := runServe(t, baseSpec(t), `{"jsonrpc":"2.0","id":9}`)
		if len(resps) != 1 {
			t.Fatalf("resps = %v", resps)
		}
		e := errorOf(t, resps[0])
		if e["code"].(float64) != -32600 || !strings.Contains(e["message"].(string), "missing method") {
			t.Errorf("error = %v", e)
		}
	})
}

// TestToolPanicIsOneErroredCall pins the recover rule: a panicking tool
// answers with an internal error and the loop keeps serving.
func TestToolPanicIsOneErroredCall(t *testing.T) {
	var out strings.Builder
	var logs strings.Builder
	s := &server{spec: baseSpec(t), out: &out, logw: &logs, tools: map[string]toolDef{}, shims: map[string]string{}}
	s.tools["boom"] = toolDef{name: "boom", call: func(json.RawMessage) (string, bool) { panic("kaboom") }}
	s.order = append(s.order, "boom")

	s.handleLine([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"boom","arguments":{}}}`))
	s.handleLine([]byte(`{"jsonrpc":"2.0","id":8,"method":"ping"}`))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses = %q", out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	e, ok := first["error"].(map[string]any)
	if !ok || e["code"].(float64) != -32603 || !strings.Contains(e["message"].(string), "failed internally") {
		t.Fatalf("panic response = %v", first)
	}
	if !strings.Contains(logs.String(), "panicked") {
		t.Errorf("panic not logged: %q", logs.String())
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if _, ok := second["result"]; !ok {
		t.Fatalf("server did not keep serving after a panic: %v", second)
	}
}

// TestNewServerNilLog pins the nil-logw convenience.
func TestNewServerNilLog(t *testing.T) {
	s, err := newServer(baseSpec(t), &strings.Builder{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.logf("must not %s", "panic")
}

// TestFSReadSeekPastEnd documents the offset-beyond-EOF behavior: an empty
// read, no truncation note (nothing was cut — there was nothing there).
func TestFSReadSeekPastEnd(t *testing.T) {
	spec := baseSpec(t)
	spec.FSRead = Access{Enabled: true}
	mustWrite(t, filepath.Join(spec.WorkspaceDir, "f.txt"), []byte("abc"))
	s := newTestServer(t, spec)
	got, isErr := s.toolFSRead(rawArgs(t, map[string]any{"path": "f.txt", "offset": 3}))
	if isErr || got != "" {
		t.Errorf("(%q, %v)", got, isErr)
	}
}

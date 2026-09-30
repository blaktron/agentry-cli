// Per-tool behavior tests: byte caps, grant enforcement, the exec shim
// jail, and the fetch egress/credential rules — the server-side halves of
// RUNNER-PLAN §R3's table, each pinned so a regression is a red test and
// not a widened capability.

package toolsrv

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func newTestServer(t *testing.T, spec *Spec) *server {
	t.Helper()
	if err := validateSpec(spec); err != nil {
		t.Fatalf("test spec invalid: %v", err)
	}
	s, err := newServer(spec, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return s
}

func rawArgs(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- fs_read ----

func TestFSRead(t *testing.T) {
	spec := baseSpec(t)
	spec.FSRead = Access{Enabled: true}
	spec.Limits.MaxReadBytes = 10
	ws := spec.WorkspaceDir
	mustWrite(t, filepath.Join(ws, "small.txt"), []byte("hello"))
	mustWrite(t, filepath.Join(ws, "big.txt"), []byte("0123456789ABCDEFGHIJ")) // 20 bytes
	mustWrite(t, filepath.Join(ws, "bin.dat"), []byte{0xff, 0xfe, 0x00, 0x01})
	mustWrite(t, filepath.Join(ws, "utf8.txt"), []byte("héllo")) // é = 2 bytes
	s := newTestServer(t, spec)

	tests := []struct {
		name     string
		args     map[string]any
		want     string
		wantErr  bool
		contains bool
	}{
		{name: "whole file", args: map[string]any{"path": "small.txt"}, want: "hello"},
		{name: "server cap with note", args: map[string]any{"path": "big.txt"},
			want: "0123456789\n[truncated at 10 bytes of 20]"},
		{name: "caller cap tighter", args: map[string]any{"path": "big.txt", "maxBytes": 3},
			want: "012\n[truncated at 3 bytes of 20]"},
		{name: "caller cap looser than server", args: map[string]any{"path": "big.txt", "maxBytes": 9999},
			want: "0123456789\n[truncated at 10 bytes of 20]"},
		{name: "offset reads the tail", args: map[string]any{"path": "big.txt", "offset": 15}, want: "FGHIJ"},
		{name: "split rune trimmed not binary", args: map[string]any{"path": "utf8.txt", "maxBytes": 2},
			want: "h\n[truncated at 1 bytes of 6]"},
		{name: "binary refused", args: map[string]any{"path": "bin.dat"},
			want: "binary file; 4 bytes", wantErr: true},
		{name: "missing path", args: map[string]any{}, want: "path is required", wantErr: true, contains: true},
		{name: "absent file", args: map[string]any{"path": "nope.txt"}, want: "nope.txt", wantErr: true, contains: true},
		{name: "directory refused", args: map[string]any{"path": "."}, want: "is a directory", wantErr: true, contains: true},
		{name: "traversal refused", args: map[string]any{"path": "../x"}, want: "escapes", wantErr: true, contains: true},
		{name: "negative offset", args: map[string]any{"path": "small.txt", "offset": -1},
			want: "offset", wantErr: true, contains: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, isErr := s.toolFSRead(rawArgs(t, tc.args))
			if isErr != tc.wantErr {
				t.Fatalf("isError = %v, want %v (text: %q)", isErr, tc.wantErr, got)
			}
			if tc.contains {
				if !strings.Contains(got, tc.want) {
					t.Fatalf("text %q does not contain %q", got, tc.want)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("text = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("paths grant enforced", func(t *testing.T) {
		gspec := baseSpec(t)
		gspec.FSRead = Access{Enabled: true, Paths: []string{"allowed"}}
		mustWrite(t, filepath.Join(gspec.WorkspaceDir, "allowed", "in.txt"), []byte("in"))
		mustWrite(t, filepath.Join(gspec.WorkspaceDir, "denied.txt"), []byte("no"))
		gs := newTestServer(t, gspec)
		if got, isErr := gs.toolFSRead(rawArgs(t, map[string]any{"path": "allowed/in.txt"})); isErr || got != "in" {
			t.Fatalf("granted read = %q (isError %v)", got, isErr)
		}
		if got, isErr := gs.toolFSRead(rawArgs(t, map[string]any{"path": "denied.txt"})); !isErr || !strings.Contains(got, "outside the granted paths") {
			t.Fatalf("ungranted read = %q (isError %v), want grant refusal", got, isErr)
		}
	})

	t.Run("disabled grant refused even on direct call", func(t *testing.T) {
		dspec := baseSpec(t)
		ds := newTestServer(t, dspec)
		if got, isErr := ds.toolFSRead(rawArgs(t, map[string]any{"path": "x"})); !isErr || !strings.Contains(got, "not granted") {
			t.Fatalf("disabled fs_read = %q (isError %v)", got, isErr)
		}
	})
}

// ---- fs_list ----

func TestFSList(t *testing.T) {
	t.Run("plain listing", func(t *testing.T) {
		spec := baseSpec(t)
		spec.FSRead = Access{Enabled: true}
		ws := spec.WorkspaceDir
		if err := os.MkdirAll(filepath.Join(ws, "adir"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(ws, "b.txt"), []byte("bb"))
		mustWrite(t, filepath.Join(ws, "c.txt"), []byte("ccc"))
		s := newTestServer(t, spec)

		got, isErr := s.toolFSList(rawArgs(t, map[string]any{}))
		if isErr {
			t.Fatalf("fs_list root: %q", got)
		}
		if want := "adir/\nb.txt\t2\nc.txt\t3"; got != want {
			t.Fatalf("fs_list = %q, want %q", got, want)
		}
		if got2, _ := s.toolFSList(rawArgs(t, map[string]any{"path": "."})); got2 != got {
			t.Fatalf("explicit \".\" = %q, want same as default %q", got2, got)
		}
		if got, isErr := s.toolFSList(rawArgs(t, map[string]any{"path": "adir"})); isErr || got != "(empty directory)" {
			t.Fatalf("empty dir = %q (isError %v)", got, isErr)
		}
		if got, isErr := s.toolFSList(rawArgs(t, map[string]any{"path": "b.txt"})); !isErr {
			t.Fatalf("listing a file succeeded: %q", got)
		}
		if got, isErr := s.toolFSList(rawArgs(t, map[string]any{"path": "../x"})); !isErr {
			t.Fatalf("traversal listing succeeded: %q", got)
		}
	})

	t.Run("paths grant: root lists the grants", func(t *testing.T) {
		spec := baseSpec(t)
		spec.FSRead = Access{Enabled: true, Paths: []string{"a/b", "c.txt"}}
		ws := spec.WorkspaceDir
		mustWrite(t, filepath.Join(ws, "a", "b", "f.txt"), []byte("ffff"))
		mustWrite(t, filepath.Join(ws, "c.txt"), []byte("ccc"))
		mustWrite(t, filepath.Join(ws, "hidden.txt"), []byte("h"))
		s := newTestServer(t, spec)

		got, isErr := s.toolFSList(rawArgs(t, map[string]any{}))
		if isErr {
			t.Fatalf("root listing: %q", got)
		}
		if want := "a/b/\nc.txt\t3"; got != want {
			t.Fatalf("root listing = %q, want the grant entries %q", got, want)
		}
		// Inside a granted dir the listing is a normal one.
		if got, isErr := s.toolFSList(rawArgs(t, map[string]any{"path": "a/b"})); isErr || got != "f.txt\t4" {
			t.Fatalf("granted dir listing = %q (isError %v)", got, isErr)
		}
		// A path outside every grant — including a grant's parent — is
		// refused rather than partially answered.
		if got, isErr := s.toolFSList(rawArgs(t, map[string]any{"path": "a"})); !isErr {
			t.Fatalf("grant-parent listing succeeded: %q", got)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		s := newTestServer(t, baseSpec(t))
		if got, isErr := s.toolFSList(rawArgs(t, map[string]any{})); !isErr || !strings.Contains(got, "not granted") {
			t.Fatalf("disabled fs_list = %q (isError %v)", got, isErr)
		}
	})
}

// ---- fs_write ----

func TestFSWrite(t *testing.T) {
	spec := baseSpec(t)
	spec.FSWrite = Access{Enabled: true}
	spec.Limits.MaxWriteBytes = 4
	s := newTestServer(t, spec)

	t.Run("creates parents and reports", func(t *testing.T) {
		got, isErr := s.toolFSWrite(rawArgs(t, map[string]any{"path": "sub/dir/f.txt", "content": "hi"}))
		if isErr || got != "wrote 2 bytes to sub/dir/f.txt" {
			t.Fatalf("fs_write = %q (isError %v)", got, isErr)
		}
		b, err := os.ReadFile(filepath.Join(spec.WorkspaceDir, "sub", "dir", "f.txt"))
		if err != nil || string(b) != "hi" {
			t.Fatalf("file content = %q, %v", b, err)
		}
	})

	t.Run("over-cap content refused, not truncated", func(t *testing.T) {
		got, isErr := s.toolFSWrite(rawArgs(t, map[string]any{"path": "big.txt", "content": "12345"}))
		if !isErr || !strings.Contains(got, "over the 4-byte limit") {
			t.Fatalf("fs_write cap = %q (isError %v)", got, isErr)
		}
		if _, err := os.Stat(filepath.Join(spec.WorkspaceDir, "big.txt")); !os.IsNotExist(err) {
			t.Fatalf("refused write still created the file (stat err %v)", err)
		}
	})

	t.Run("traversal refused", func(t *testing.T) {
		if got, isErr := s.toolFSWrite(rawArgs(t, map[string]any{"path": "../x", "content": "a"})); !isErr {
			t.Fatalf("traversal write succeeded: %q", got)
		}
	})

	t.Run("paths grant enforced", func(t *testing.T) {
		gspec := baseSpec(t)
		gspec.FSWrite = Access{Enabled: true, Paths: []string{"out"}}
		gs := newTestServer(t, gspec)
		if got, isErr := gs.toolFSWrite(rawArgs(t, map[string]any{"path": "out/x.txt", "content": "a"})); isErr {
			t.Fatalf("granted write refused: %q", got)
		}
		if got, isErr := gs.toolFSWrite(rawArgs(t, map[string]any{"path": "other/x.txt", "content": "a"})); !isErr || !strings.Contains(got, "outside the granted paths") {
			t.Fatalf("ungranted write = %q (isError %v)", got, isErr)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		ds := newTestServer(t, baseSpec(t))
		if got, isErr := ds.toolFSWrite(rawArgs(t, map[string]any{"path": "x", "content": "a"})); !isErr || !strings.Contains(got, "not granted") {
			t.Fatalf("disabled fs_write = %q (isError %v)", got, isErr)
		}
	})
}

// ---- write_report ----

func TestWriteReport(t *testing.T) {
	spec := baseSpec(t)
	s := newTestServer(t, spec)

	t.Run("nested write", func(t *testing.T) {
		got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "sub/r.md", "content": "hello"}))
		if isErr || got != "report written: reports/sub/r.md (5 bytes)" {
			t.Fatalf("write_report = %q (isError %v)", got, isErr)
		}
		b, err := os.ReadFile(filepath.Join(spec.OutDir, "reports", "sub", "r.md"))
		if err != nil || string(b) != "hello" {
			t.Fatalf("report content = %q, %v", b, err)
		}
	})

	t.Run("append extends", func(t *testing.T) {
		if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "a.log", "content": "one\n"})); isErr {
			t.Fatalf("first write: %q", got)
		}
		got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "a.log", "content": "two\n", "append": true}))
		if isErr || got != "report written: reports/a.log (4 bytes)" {
			t.Fatalf("append = %q (isError %v)", got, isErr)
		}
		b, err := os.ReadFile(filepath.Join(spec.OutDir, "reports", "a.log"))
		if err != nil || string(b) != "one\ntwo\n" {
			t.Fatalf("appended content = %q, %v", b, err)
		}
	})

	t.Run("plain rewrite replaces", func(t *testing.T) {
		_, _ = s.toolWriteReport(rawArgs(t, map[string]any{"path": "b.log", "content": "first"}))
		_, _ = s.toolWriteReport(rawArgs(t, map[string]any{"path": "b.log", "content": "second"}))
		b, _ := os.ReadFile(filepath.Join(spec.OutDir, "reports", "b.log"))
		if string(b) != "second" {
			t.Fatalf("rewrite content = %q, want %q", b, "second")
		}
	})

	t.Run("traversal refused", func(t *testing.T) {
		if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"path": "../escape", "content": "x"})); !isErr {
			t.Fatalf("traversal report succeeded: %q", got)
		}
	})

	t.Run("cap refused", func(t *testing.T) {
		cspec := baseSpec(t)
		cspec.Limits.MaxWriteBytes = 4
		cs := newTestServer(t, cspec)
		if got, isErr := cs.toolWriteReport(rawArgs(t, map[string]any{"path": "r", "content": "12345"})); !isErr || !strings.Contains(got, "over the 4-byte limit") {
			t.Fatalf("cap = %q (isError %v)", got, isErr)
		}
	})

	t.Run("missing path", func(t *testing.T) {
		if got, isErr := s.toolWriteReport(rawArgs(t, map[string]any{"content": "x"})); !isErr {
			t.Fatalf("pathless report succeeded: %q", got)
		}
	})
}

// ---- run_command ----

func TestRunCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec tests use sh/echo semantics")
	}
	spec := baseSpec(t)
	spec.Exec = Exec{Commands: []string{"sh", "echo", "agentry-no-such-cmd"}}
	s := newTestServer(t, spec)
	if s.shimDir == "" {
		t.Fatal("shim dir was not built")
	}

	t.Run("declared command runs", func(t *testing.T) {
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "echo", "args": []string{"hello"}}))
		if isErr || got != "exit code 0\nhello\n" {
			t.Fatalf("echo = %q (isError %v)", got, isErr)
		}
	})

	t.Run("runs without a workspace mount", func(t *testing.T) {
		// exec with no filesystem grant: the workspace dir does not exist
		// (agentry-cli#120), the command still runs, in a scratch dir.
		noWS := baseSpec(t)
		noWS.WorkspaceDir = filepath.Join(t.TempDir(), "never-mounted")
		noWS.Exec = Exec{Commands: []string{"sh"}}
		s2 := newTestServer(t, noWS)
		got, isErr := s2.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "pwd"}}))
		if isErr || !strings.HasPrefix(got, "exit code 0\n") || strings.Contains(got, "never-mounted") {
			t.Fatalf("run_command without a workspace = %q (isError %v)", got, isErr)
		}
	})

	t.Run("args sent as a JSON string are the array they encode", func(t *testing.T) {
		// agentry-cli#113: what the operator's provider sends.
		got, isErr := s.toolRunCommand(json.RawMessage(`{"command":"echo","args":"[\"hello\",\"there\"]"}`))
		if isErr || got != "exit code 0\nhello there\n" {
			t.Fatalf("echo with stringified args = %q (isError %v)", got, isErr)
		}
	})

	t.Run("non-zero exit is a result, not an error", func(t *testing.T) {
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "exit 3"}}))
		if isErr {
			t.Fatalf("non-zero exit reported as tool error: %q", got)
		}
		if got != "exit code 3\n" {
			t.Fatalf("text = %q, want %q", got, "exit code 3\n")
		}
	})

	t.Run("child PATH is only the shim dir", func(t *testing.T) {
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", `echo "$PATH|$HOME"`}}))
		if isErr {
			t.Fatalf("sh: %q", got)
		}
		if want := "exit code 0\n" + s.shimDir + "|/tmp\n"; got != want {
			t.Fatalf("env probe = %q, want %q", got, want)
		}
	})

	t.Run("cwd is the workspace", func(t *testing.T) {
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "sh", "args": []string{"-c", "pwd"}}))
		if isErr {
			t.Fatalf("pwd: %q", got)
		}
		want, err := filepath.EvalSymlinks(spec.WorkspaceDir)
		if err != nil {
			t.Fatal(err)
		}
		if got != "exit code 0\n"+want+"\n" {
			t.Fatalf("pwd = %q, want workspace %q", got, want)
		}
	})

	t.Run("stdin is piped", func(t *testing.T) {
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{
			"command": "sh", "args": []string{"-c", `read x; echo "got:$x"`}, "stdin": "ping\n",
		}))
		if isErr || got != "exit code 0\ngot:ping\n" {
			t.Fatalf("stdin probe = %q (isError %v)", got, isErr)
		}
	})

	t.Run("undeclared command refused", func(t *testing.T) {
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "cat"}))
		if !isErr || !strings.Contains(got, "not in the declared exec allowlist") {
			t.Fatalf("undeclared = %q (isError %v)", got, isErr)
		}
	})

	t.Run("path separators refused", func(t *testing.T) {
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "bin/sh"}))
		if !isErr || !strings.Contains(got, "bare command name") {
			t.Fatalf("separator = %q (isError %v)", got, isErr)
		}
	})

	t.Run("declared but absent command", func(t *testing.T) {
		got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "agentry-no-such-cmd"}))
		if !isErr || !strings.Contains(got, "not present in image") {
			t.Fatalf("absent = %q (isError %v)", got, isErr)
		}
	})

	t.Run("output capped with note", func(t *testing.T) {
		cspec := baseSpec(t)
		cspec.Exec = Exec{Commands: []string{"echo"}}
		cspec.Limits.MaxExecOutputBytes = 5
		cs := newTestServer(t, cspec)
		got, isErr := cs.toolRunCommand(rawArgs(t, map[string]any{"command": "echo", "args": []string{"hellothere"}}))
		if isErr {
			t.Fatalf("echo: %q", got)
		}
		if want := "exit code 0\nhello\n[output truncated at 5 bytes of 11]"; got != want {
			t.Fatalf("capped output = %q, want %q", got, want)
		}
	})

	t.Run("timeout clamps and kills", func(t *testing.T) {
		tspec := baseSpec(t)
		tspec.Exec = Exec{Commands: []string{"sh"}}
		tspec.Limits.ExecTimeoutSeconds = 1
		ts := newTestServer(t, tspec)
		// timeoutSeconds far above the server limit: min() must win.
		got, isErr := ts.toolRunCommand(rawArgs(t, map[string]any{
			"command": "sh", "args": []string{"-c", "while :; do :; done"}, "timeoutSeconds": 9999,
		}))
		if !isErr || !strings.Contains(got, "timed out after 1s") {
			t.Fatalf("timeout = %q (isError %v)", got, isErr)
		}
	})

	t.Run("exec not granted", func(t *testing.T) {
		ns := newTestServer(t, baseSpec(t))
		if got, isErr := ns.toolRunCommand(rawArgs(t, map[string]any{"command": "sh"})); !isErr || !strings.Contains(got, "not granted") {
			t.Fatalf("ungranted exec = %q (isError %v)", got, isErr)
		}
	})
}

// ---- fetch ----

func TestFetch(t *testing.T) {
	const secret = "sekret-token-value-8f2"
	t.Setenv("AGENTRY_TEST_TOKEN", secret)
	t.Setenv("AGENTRY_EMPTY_TOKEN", "")
	t.Setenv("AGENTRY_UNGRANTED_TOKEN", "other-value")

	// Second upstream, addressed as "localhost" so its hostname differs
	// from the allowlisted "127.0.0.1" — the redirect gate must stop the
	// hop before this handler ever runs.
	var hitB atomic.Bool
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitB.Store(true)
	}))
	defer upstreamB.Close()
	bu, err := url.Parse(upstreamB.URL)
	if err != nil {
		t.Fatal(err)
	}
	redirTarget := "http://localhost:" + bu.Port() + "/x"

	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			_, _ = fmt.Fprint(w, "hello from upstream")
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			_, _ = fmt.Fprintf(w, "method=%s body=%s", r.Method, body)
		case "/auth":
			_, _ = fmt.Fprintf(w, "auth=%q apikey=%q", r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
		case "/redirect":
			http.Redirect(w, r, redirTarget, http.StatusFound)
		case "/big":
			_, _ = fmt.Fprint(w, strings.Repeat("a", 100))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstreamA.Close()

	spec := baseSpec(t)
	spec.Fetch = Fetch{
		Enabled:   true,
		Hosts:     []string{"127.0.0.1"},
		SecretEnv: []string{"AGENTRY_TEST_TOKEN", "AGENTRY_EMPTY_TOKEN"},
	}
	s := newTestServer(t, spec)

	t.Run("allowed host", func(t *testing.T) {
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": upstreamA.URL + "/hello"}))
		if isErr {
			t.Fatalf("fetch: %q", got)
		}
		if !strings.Contains(got, "HTTP 200 OK") || !strings.Contains(got, "hello from upstream") {
			t.Fatalf("response text = %q", got)
		}
	})

	t.Run("disallowed host refused before dialing", func(t *testing.T) {
		au, _ := url.Parse(upstreamA.URL)
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": "http://localhost:" + au.Port() + "/hello"}))
		if !isErr || !strings.Contains(got, "not in the egress allowlist") {
			t.Fatalf("disallowed = %q (isError %v)", got, isErr)
		}
	})

	t.Run("method normalized and body sent", func(t *testing.T) {
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": upstreamA.URL + "/echo", "method": "post", "body": "ping"}))
		if isErr || !strings.Contains(got, "method=POST body=ping") {
			t.Fatalf("post = %q (isError %v)", got, isErr)
		}
	})

	t.Run("unlisted method refused", func(t *testing.T) {
		if got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": upstreamA.URL, "method": "BREW"})); !isErr || !strings.Contains(got, "not allowed") {
			t.Fatalf("method = %q (isError %v)", got, isErr)
		}
	})

	t.Run("non-http scheme refused", func(t *testing.T) {
		if got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": "ftp://example.com/x"})); !isErr || !strings.Contains(got, "scheme") {
			t.Fatalf("scheme = %q (isError %v)", got, isErr)
		}
	})

	t.Run("manual credential headers refused", func(t *testing.T) {
		for _, h := range []string{"Authorization", "authorization", "cookie", "Proxy-Authorization"} {
			got, isErr := s.toolFetch(rawArgs(t, map[string]any{
				"url": upstreamA.URL, "headers": map[string]string{h: "x"},
			}))
			if !isErr || !strings.Contains(got, "auth parameter") {
				t.Fatalf("header %q = %q (isError %v), want refusal", h, got, isErr)
			}
		}
	})

	t.Run("bearer auth injected and scrubbed", func(t *testing.T) {
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{
			"url": upstreamA.URL + "/auth", "auth": map[string]any{"env": "AGENTRY_TEST_TOKEN"},
		}))
		if isErr {
			t.Fatalf("auth fetch: %q", got)
		}
		if !strings.Contains(got, `auth="Bearer [redacted:AGENTRY_TEST_TOKEN]"`) {
			t.Fatalf("upstream did not see the bearer header (or scrub failed): %q", got)
		}
		if strings.Contains(got, secret) {
			t.Fatalf("secret value leaked into the response text: %q", got)
		}
	})

	t.Run("header-scheme auth injected and scrubbed", func(t *testing.T) {
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{
			"url":  upstreamA.URL + "/auth",
			"auth": map[string]any{"env": "AGENTRY_TEST_TOKEN", "scheme": "header", "header": "X-Api-Key"},
		}))
		if isErr || !strings.Contains(got, `apikey="[redacted:AGENTRY_TEST_TOKEN]"`) {
			t.Fatalf("header auth = %q (isError %v)", got, isErr)
		}
	})

	t.Run("auth env must be granted", func(t *testing.T) {
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{
			"url": upstreamA.URL + "/auth", "auth": map[string]any{"env": "AGENTRY_UNGRANTED_TOKEN"},
		}))
		if !isErr || !strings.Contains(got, "not a granted secret") {
			t.Fatalf("ungranted auth = %q (isError %v)", got, isErr)
		}
	})

	t.Run("empty secret reported", func(t *testing.T) {
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{
			"url": upstreamA.URL + "/auth", "auth": map[string]any{"env": "AGENTRY_EMPTY_TOKEN"},
		}))
		if !isErr || !strings.Contains(got, "secret AGENTRY_EMPTY_TOKEN not set") {
			t.Fatalf("empty secret = %q (isError %v)", got, isErr)
		}
	})

	t.Run("unknown auth scheme refused", func(t *testing.T) {
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{
			"url": upstreamA.URL, "auth": map[string]any{"env": "AGENTRY_TEST_TOKEN", "scheme": "basic"},
		}))
		if !isErr || !strings.Contains(got, "unknown auth.scheme") {
			t.Fatalf("scheme = %q (isError %v)", got, isErr)
		}
	})

	t.Run("redirect to disallowed host refused", func(t *testing.T) {
		got, isErr := s.toolFetch(rawArgs(t, map[string]any{"url": upstreamA.URL + "/redirect"}))
		if !isErr || !strings.Contains(got, `redirect to host "localhost" refused`) {
			t.Fatalf("redirect = %q (isError %v)", got, isErr)
		}
		if hitB.Load() {
			t.Fatal("the disallowed redirect target was actually contacted")
		}
	})

	t.Run("response capped with note", func(t *testing.T) {
		cspec := baseSpec(t)
		cspec.Fetch = Fetch{Enabled: true, Hosts: []string{"127.0.0.1"}}
		cspec.Limits.MaxFetchBytes = 10
		cs := newTestServer(t, cspec)
		got, isErr := cs.toolFetch(rawArgs(t, map[string]any{"url": upstreamA.URL + "/big"}))
		if isErr {
			t.Fatalf("big fetch: %q", got)
		}
		if !strings.Contains(got, "\n\naaaaaaaaaa\n[truncated at 10 bytes of 100]") {
			t.Fatalf("truncated fetch = %q", got)
		}
	})

	t.Run("allowAll bypasses the host list", func(t *testing.T) {
		wspec := baseSpec(t)
		wspec.Fetch = Fetch{Enabled: true, AllowAll: true}
		wsrv := newTestServer(t, wspec)
		got, isErr := wsrv.toolFetch(rawArgs(t, map[string]any{"url": upstreamA.URL + "/hello"}))
		if isErr || !strings.Contains(got, "hello from upstream") {
			t.Fatalf("allowAll fetch = %q (isError %v)", got, isErr)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		ds := newTestServer(t, baseSpec(t))
		if got, isErr := ds.toolFetch(rawArgs(t, map[string]any{"url": upstreamA.URL})); !isErr || !strings.Contains(got, "not granted") {
			t.Fatalf("disabled fetch = %q (isError %v)", got, isErr)
		}
	})

	t.Run("missing url", func(t *testing.T) {
		if got, isErr := s.toolFetch(rawArgs(t, map[string]any{})); !isErr || !strings.Contains(got, "url is required") {
			t.Fatalf("urlless fetch = %q (isError %v)", got, isErr)
		}
	})
}

// run_command's output passes fetch's secret scrub (agentry-cli#246): a
// granted secret's value never reaches the model verbatim through exec.
func TestRunCommandScrubsGrantedSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec tests use sh/echo semantics")
	}
	t.Setenv("AGENTRY_T246_TOKEN", "tok-246-value")
	spec := baseSpec(t)
	spec.Exec = Exec{Commands: []string{"echo"}}
	spec.Fetch.SecretEnv = []string{"AGENTRY_T246_TOKEN"}
	s := newTestServer(t, spec)
	got, isErr := s.toolRunCommand(rawArgs(t, map[string]any{"command": "echo", "args": []string{"before tok-246-value after"}}))
	if isErr {
		t.Fatalf("echo: %q", got)
	}
	if want := "exit code 0\nbefore [redacted:AGENTRY_T246_TOKEN] after\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// In Unsafe Mode (Exec.ImagePath; agentry-cli#490, the operator's decision
// of 2026-09-30) a declared command runs with the image's PATH after the
// shims, so a declared script finds the interpreter and programs it calls.
// The argv[0] allowlist does not move: an undeclared bash is still refused
// (plan unsafe-mode.md invariant 2). Off, the PATH is the shims alone.
func TestRunCommandImagePathInUnsafeModeOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec tests use sh semantics")
	}
	// A declared script whose shebang looks its interpreter up on PATH,
	// the way Ghidra's analyzeHeadless does.
	dir := t.TempDir()
	script := filepath.Join(dir, "declared-script")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env sh\necho ran \"$(basename \"$(command -v cat)\")\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	jailed := baseSpec(t)
	jailed.Exec = Exec{Commands: []string{"declared-script"}}
	got, _ := newTestServer(t, jailed).toolRunCommand(rawArgs(t, map[string]any{"command": "declared-script"}))
	if strings.Contains(got, "ran cat") {
		t.Fatalf("under the shim PATH a declared script found sh and cat: %q", got)
	}

	unsafe := baseSpec(t)
	unsafe.Exec = Exec{Commands: []string{"declared-script"}, ImagePath: true}
	us := newTestServer(t, unsafe)
	got, isErr := us.toolRunCommand(rawArgs(t, map[string]any{"command": "declared-script"}))
	if isErr || got != "exit code 0\nran cat\n" {
		t.Fatalf("with the image PATH = %q (isError %v)", got, isErr)
	}
	for _, undeclared := range []string{"bash", "sh", "cat"} {
		got, isErr := us.toolRunCommand(rawArgs(t, map[string]any{"command": undeclared}))
		if !isErr || !strings.Contains(got, "not in the declared exec allowlist") {
			t.Fatalf("undeclared %s with the image PATH = %q (isError %v)", undeclared, got, isErr)
		}
	}
}

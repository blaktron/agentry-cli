package bridge

import (
	"bytes"
	"context"
	"encoding/json"
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
)

const initLine = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"gateway","version":"1"}}}`

// lockedBuffer is the bridge's stdout in a test: written by the bridge's
// goroutines, read by the test while the bridge runs.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// messages are the lines the bridge wrote, each decoded.
func (l *lockedBuffer) messages(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("the bridge wrote a line that is not one JSON message: %q", line)
		}
		out = append(out, m)
	}
	return out
}

// byID is the message answering id (a JSON number in these tests).
func byID(t *testing.T, msgs []map[string]any, id float64) map[string]any {
	t.Helper()
	for _, m := range msgs {
		if v, ok := m["id"].(float64); ok && v == id && m["method"] == nil {
			return m
		}
	}
	t.Fatalf("no answer to id %v in %v", id, msgs)
	return nil
}

func newBridge(t *testing.T, srv *httptest.Server, tokenFile string, out io.Writer, log io.Writer) *Bridge {
	t.Helper()
	client := srv.Client()
	client.CheckRedirect = DefaultClient().CheckRedirect
	b, err := New(Config{URL: srv.URL + "/mcp", TokenFile: tokenFile, Client: client, Log: log}, out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeToken(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

// streamable is a streamable HTTP server: it names a session at
// initialize, checks the session and the protocol version on every later
// call, records each Authorization header, and answers tools/list.
type streamable struct {
	mu      sync.Mutex
	auths   []string
	deleted bool
	sse     bool
}

func (s *streamable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.mu.Unlock()
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		s.deleted = r.Header.Get(sessionHeader) == "sess-1"
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &msg)
	if msg.Method != "initialize" {
		if r.Header.Get(sessionHeader) != "sess-1" || r.Header.Get(protocolHeader) != "2025-06-18" {
			http.Error(w, "no session", http.StatusBadRequest)
			return
		}
	}
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result string
	switch msg.Method {
	case "initialize":
		w.Header().Set(sessionHeader, "sess-1")
		result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"remote","version":"1"}}`
	case "tools/list":
		result = `{"tools":[{"name":"list_repos","inputSchema":{"type":"object"}}]}`
	default:
		result = `{}`
	}
	answer := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, msg.ID, result)
	if s.sse && msg.Method != "initialize" {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\n")
		_, _ = fmt.Fprint(w, "data: \"params\":{\"progressToken\":1,\"progress\":1}}\n\n")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", answer)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, answer)
}

func TestStreamableForwardsWithSessionProtocolAndToken(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			remote := &streamable{sse: sse}
			srv := httptest.NewTLSServer(remote)
			defer srv.Close()
			tok := filepath.Join(t.TempDir(), "bearer")
			writeToken(t, tok, "first-token-value\n")
			var out, log lockedBuffer
			b := newBridge(t, srv, tok, &out, &log)
			in := strings.Join([]string{
				initLine,
				`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
			}, "\n") + "\n"
			if err := b.Serve(context.Background(), strings.NewReader(in)); err != nil {
				t.Fatal(err)
			}
			msgs := out.messages(t)
			if r := byID(t, msgs, 1)["result"].(map[string]any); r["protocolVersion"] != "2025-06-18" {
				t.Fatalf("initialize answer not passed on: %v", r)
			}
			list := byID(t, msgs, 2)
			if list["error"] != nil {
				t.Fatalf("tools/list failed: %v (log %s)", list["error"], log.String())
			}
			if !strings.Contains(out.String(), "list_repos") {
				t.Fatalf("tools/list answer not passed on: %s", out.String())
			}
			if sse && !strings.Contains(out.String(), "notifications/progress") {
				t.Fatalf("the progress notification in the event stream was not passed on: %s", out.String())
			}
			remote.mu.Lock()
			defer remote.mu.Unlock()
			for _, a := range remote.auths {
				if a != "Bearer first-token-value" {
					t.Fatalf("a request carried %q, want the file's bearer", a)
				}
			}
			if !remote.deleted {
				t.Fatal("the session was not ended with a DELETE when the gateway closed stdin")
			}
		})
	}
}

func TestTokenIsReadAtEveryRequest(t *testing.T) {
	remote := &streamable{}
	srv := httptest.NewTLSServer(remote)
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "bearer")
	writeToken(t, tok, "token-before-renewal")
	var out lockedBuffer
	b := newBridge(t, srv, tok, &out, io.Discard)
	inR, inW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- b.Serve(context.Background(), inR) }()
	_, _ = fmt.Fprintln(inW, initLine)
	waitFor(t, &out, `"id":1`)
	// The runner renews the token mid-run: an atomic rename, as M3 will.
	next := tok + ".new"
	writeToken(t, next, "token-after-renewal")
	if err := os.Rename(next, tok); err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	waitFor(t, &out, `"id":2`)
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.auths[0] != "Bearer token-before-renewal" || remote.auths[1] != "Bearer token-after-renewal" {
		t.Fatalf("the bearer was not read again at the second request: %q", remote.auths)
	}
}

func TestEmptyTokenFileSendsNoAuthorization(t *testing.T) {
	remote := &streamable{}
	srv := httptest.NewTLSServer(remote)
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "bearer")
	writeToken(t, tok, "")
	b := newBridge(t, srv, tok, io.Discard, io.Discard)
	if err := b.Serve(context.Background(), strings.NewReader(initLine+"\n")); err != nil {
		t.Fatal(err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.auths[0] != "" {
		t.Fatalf("an empty credential file sent %q", remote.auths[0])
	}
}

func TestRefusalsEndTheCallWithASentenceNamingTheServer(t *testing.T) {
	cases := []struct {
		status    int
		withToken bool
		want      string
	}{
		{http.StatusUnauthorized, true, "did not accept the credential the run sent (HTTP 401)"},
		{http.StatusUnauthorized, false, "asks for a sign-in (HTTP 401), and this QuickAgent declares it with auth: none"},
		{http.StatusForbidden, true, "refused the call with the credential the run sent (HTTP 403)"},
		{http.StatusFound, true, "follows no redirect"},
		{http.StatusInternalServerError, true, "answered 500 Internal Server Error"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.status, c.withToken), func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.status == http.StatusFound {
					http.Redirect(w, r, "https://elsewhere.example/mcp", http.StatusFound)
					return
				}
				// A server that echoes what it was sent must not get it into
				// the sentence.
				http.Error(w, r.Header.Get("Authorization"), c.status)
			}))
			defer srv.Close()
			tok := ""
			if c.withToken {
				tok = filepath.Join(t.TempDir(), "bearer")
				writeToken(t, tok, "secret-bearer-value")
			}
			var out, log lockedBuffer
			b := newBridge(t, srv, tok, &out, &log)
			in := initLine + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"x"}}` + "\n"
			if err := b.Serve(context.Background(), strings.NewReader(in)); err != nil {
				t.Fatal(err)
			}
			msgs := out.messages(t)
			for _, id := range []float64{1, 2} {
				e, _ := byID(t, msgs, id)["error"].(map[string]any)
				if e == nil {
					t.Fatalf("id %v: no error: %v", id, msgs)
				}
				msg, _ := e["message"].(string)
				if !strings.HasPrefix(msg, "the remote MCP server 127.0.0.1 ") || !strings.Contains(msg, c.want) {
					t.Fatalf("id %v: error %q does not name the server and say %q", id, msg, c.want)
				}
			}
			if strings.Contains(out.String()+log.String(), "secret-bearer-value") {
				t.Fatal("the bearer value reached the gateway's stream or the log")
			}
		})
	}
}

func TestBatchAnswerIsSplitIntoMessages(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{"jsonrpc":"2.0","method":"notifications/message","params":{}},{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}]`)
	}))
	defer srv.Close()
	var out lockedBuffer
	b := newBridge(t, srv, "", &out, io.Discard)
	if err := b.Serve(context.Background(), strings.NewReader(initLine+"\n")); err != nil {
		t.Fatal(err)
	}
	if n := len(out.messages(t)); n != 2 {
		t.Fatalf("a batch of two became %d lines", n)
	}
	if b.protocol != "2025-03-26" {
		t.Fatalf("the agreed protocol version was not learned: %q", b.protocol)
	}
}

func TestEventStreamEndingWithoutTheAnswerEndsTheCall(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
	}))
	defer srv.Close()
	var out lockedBuffer
	b := newBridge(t, srv, "", &out, io.Discard)
	if err := b.Serve(context.Background(), strings.NewReader(initLine+"\n")); err != nil {
		t.Fatal(err)
	}
	e, _ := byID(t, out.messages(t), 1)["error"].(map[string]any)
	if e == nil || !strings.Contains(e["message"].(string), "without an answer") {
		t.Fatalf("a stream that ended without the answer left the call open: %s", out.String())
	}
}

// legacy is an HTTP+SSE (2024-11-05) server: POST to its address is 405,
// GET opens the stream, whose first event names the message address, and
// every answer comes back on the stream.
type legacy struct {
	mu       sync.Mutex
	stream   chan string
	auths    []string
	endpoint string
}

func (l *legacy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.auths = append(l.auths, r.Header.Get("Authorization"))
	l.mu.Unlock()
	switch {
	case r.URL.Path == "/mcp" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusMethodNotAllowed)
	case r.URL.Path == "/mcp" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", l.endpoint)
		w.(http.Flusher).Flush()
		for msg := range l.stream {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			w.(http.Flusher).Flush()
		}
	case r.URL.Path == "/messages" && r.Method == http.MethodPost:
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		w.WriteHeader(http.StatusAccepted)
		if len(msg.ID) != 0 {
			l.stream <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, msg.ID)
		}
	default:
		http.NotFound(w, r)
	}
}

func TestLegacySSETransportFallback(t *testing.T) {
	remote := &legacy{stream: make(chan string, 4), endpoint: "/messages?session=abc"}
	srv := httptest.NewTLSServer(remote)
	defer srv.Close()
	defer close(remote.stream)
	tok := filepath.Join(t.TempDir(), "bearer")
	writeToken(t, tok, "legacy-token")
	var out lockedBuffer
	b := newBridge(t, srv, tok, &out, io.Discard)
	b.cfg.Lost = func(error) {}
	inR, inW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- b.Serve(context.Background(), inR) }()
	_, _ = fmt.Fprintln(inW, initLine)
	waitFor(t, &out, `"id":1`)
	_, _ = fmt.Fprintln(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	waitFor(t, &out, `"id":2`)
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	remote.mu.Lock()
	defer remote.mu.Unlock()
	for _, a := range remote.auths {
		if a != "Bearer legacy-token" {
			t.Fatalf("a legacy request carried %q", a)
		}
	}
}

func TestLegacyEndpointOnAnotherOriginIsRefused(t *testing.T) {
	remote := &legacy{stream: make(chan string), endpoint: "https://elsewhere.example/messages"}
	srv := httptest.NewTLSServer(remote)
	defer srv.Close()
	defer close(remote.stream)
	b := newBridge(t, srv, "", io.Discard, io.Discard)
	b.cfg.Lost = func(error) {}
	err := b.Serve(context.Background(), strings.NewReader(initLine+"\n"))
	if err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("an endpoint on another origin was not refused: %v", err)
	}
}

func TestNewRefusesAnAddressThatIsNotHTTPS(t *testing.T) {
	for _, u := range []string{"http://example.com/mcp", "https://user:pw@example.com/mcp", "https:///mcp", "https://example.com/mcp#x"} {
		if _, err := New(Config{URL: u}, io.Discard); err == nil {
			t.Errorf("%s was accepted", u)
		}
	}
}

func TestAMalformedTokenIsNeverSent(t *testing.T) {
	remote := &streamable{}
	srv := httptest.NewTLSServer(remote)
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "bearer")
	writeToken(t, tok, "two\nlines")
	var out lockedBuffer
	b := newBridge(t, srv, tok, &out, io.Discard)
	if err := b.Serve(context.Background(), strings.NewReader(initLine+"\n")); err != nil {
		t.Fatal(err)
	}
	e, _ := byID(t, out.messages(t), 1)["error"].(map[string]any)
	if e == nil || !strings.Contains(e["message"].(string), "not a single printable token") {
		t.Fatalf("a token with a line break was not refused: %s", out.String())
	}
	if len(remote.auths) != 0 {
		t.Fatal("a request was sent with a malformed credential")
	}
}

func waitFor(t *testing.T, out *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s; the bridge wrote %q", want, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The bridge reaches its server by the host's own name, through the relay
// the gateway links under it: a proxy variable in its environment must not
// send the call, and its bearer, anywhere else.
func TestDefaultClientTakesNoProxyAndFollowsNoRedirect(t *testing.T) {
	c := DefaultClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("the bridge's transport reads a proxy: %#v", c.Transport)
	}
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("the bridge's client follows redirects")
	}
}

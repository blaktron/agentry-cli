// End-to-end tests of the CONNECT allowlist: a permitted tunnel must
// carry bytes both ways, a denied one must die with a 403 and a receipt
// line, and nothing but CONNECT may pass — the proxy is the single hole
// in the agent container's network isolation, so its
// behavior is pinned at the socket level, not through a mock.

package netproxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuf is a race-safe log sink: handler goroutines write while the
// test goroutine reads.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startEcho runs a raw TCP upstream that echoes whatever it receives —
// the stand-in for a model endpoint on the far side of the tunnel.
func startEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// startProxy serves p on a loopback listener and returns its address.
func startProxy(t *testing.T, p *Proxy) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: p.Handler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// dialProxy opens a client conn, sends one raw request line + headers,
// and returns the conn, a reader positioned after the response headers,
// and the status line. host goes into the Host header — for CONNECT it is
// the target itself, for other methods a well-formed authority (Go's
// server 400s a malformed Host before any handler runs).
func dialProxy(t *testing.T, proxyAddr, method, target, host string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: %s\r\n\r\n", method, target, host); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	for { // drain response headers
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if l == "\r\n" {
			break
		}
	}
	return c, br, strings.TrimSpace(status)
}

func TestConnectTunnelEndToEnd(t *testing.T) {
	echo := startTLSEcho(t)
	target := echo.Addr().String()
	logs := &lockedBuf{}
	addr := startProxy(t, &Proxy{Allow: []string{target}, Logw: logs})

	// Bytes must round-trip through the tunnel in both directions, over
	// TLS: a tunnel carries nothing but a ClientHello first (sni.go).
	if err := tlsThrough(openTunnel(t, addr, target), "127.0.0.1"); err != nil {
		t.Fatalf("TLS through the tunnel: %v", err)
	}
	if !strings.Contains(logs.String(), "allow "+target) {
		t.Fatalf("log missing allow line: %q", logs.String())
	}
}

func TestConnectDenied(t *testing.T) {
	logs := &lockedBuf{}
	addr := startProxy(t, &Proxy{Allow: []string{"api.anthropic.com"}, Logw: logs})

	_, _, status := dialProxy(t, addr, http.MethodConnect, "127.0.0.1:9", "127.0.0.1:9")
	if !strings.HasPrefix(status, "HTTP/1.1 403") {
		t.Fatalf("status = %q, want 403", status)
	}
	if !strings.Contains(logs.String(), "deny 127.0.0.1:9") {
		t.Fatalf("log missing deny line: %q", logs.String())
	}
}

func TestNonConnectRefused(t *testing.T) {
	addr := startProxy(t, &Proxy{Allow: []string{"api.anthropic.com"}})
	_, _, status := dialProxy(t, addr, http.MethodGet, "http://example.com/", "example.com")
	if !strings.HasPrefix(status, "HTTP/1.1 405") {
		t.Fatalf("status = %q, want 405", status)
	}
}

func TestPermits(t *testing.T) {
	p := &Proxy{Allow: []string{"api.anthropic.com", "Generate.Example.COM:8443", "127.0.0.1:9999", " spaced.example.com "}}
	tests := []struct {
		host, port string
		want       bool
	}{
		{"api.anthropic.com", "443", true}, // entry without port defaults to 443
		{"api.anthropic.com", "80", false},
		{"API.ANTHROPIC.COM", "443", true}, // hosts compare case-insensitively
		{"generate.example.com", "8443", true},
		{"generate.example.com", "443", false},
		{"127.0.0.1", "9999", true},
		{"127.0.0.1", "443", false},
		{"spaced.example.com", "443", true},
		{"evil.example.com", "443", false},
		{"anthropic.com", "443", false}, // no suffix matching
	}
	for _, tc := range tests {
		if got := p.permits(tc.host, tc.port); got != tc.want {
			t.Errorf("permits(%q, %q) = %v, want %v", tc.host, tc.port, got, tc.want)
		}
	}

	empty := &Proxy{}
	if empty.permits("anything.example.com", "443") {
		t.Error("zero-value proxy permitted a tunnel; it must deny everything")
	}
}

// freePort reserves an ephemeral loopback port and releases it, so
// ListenAndServe can be exercised on its real addr-string interface.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestListenAndServe drives the production entry point (not the
// test-facing Handler) end to end: it must announce itself, bind the addr,
// and carry a permitted tunnel.
func TestListenAndServe(t *testing.T) {
	echo := startTLSEcho(t)
	target := echo.Addr().String()
	logs := &lockedBuf{}
	p := &Proxy{Allow: []string{target}, Logw: logs}
	addr := freePort(t)
	go func() { _ = p.ListenAndServe(addr) }()

	// The goroutine owns the bind; poll until the socket answers.
	var conn net.Conn
	var err error
	for i := 0; i < 100; i++ {
		conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("proxy never came up on %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("status = %q, err %v", status, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == "\r\n" {
			break
		}
	}
	if err := tlsThrough(tunnelConn{Conn: conn, r: br}, "127.0.0.1"); err != nil {
		t.Fatalf("TLS through the ListenAndServe tunnel: %v", err)
	}
	if !strings.Contains(logs.String(), "llm-proxy listening on "+addr) {
		t.Errorf("startup line missing from receipts log: %q", logs.String())
	}
}

// TestListenAndServeBadAddr pins the error return: a bind failure must
// surface, not hang.
func TestListenAndServeBadAddr(t *testing.T) {
	p := &Proxy{}
	if err := p.ListenAndServe("127.0.0.1:not-a-port"); err == nil {
		t.Fatal("bad addr must error")
	}
}

// TestConnectDialFailure pins the 502 branch: an allowlisted target that
// cannot be reached is a bad gateway, not a silent hang.
func TestConnectDialFailure(t *testing.T) {
	// Reserve-and-release: the port is (almost certainly) closed now.
	closed := freePort(t)
	logs := &lockedBuf{}
	addr := startProxy(t, &Proxy{Allow: []string{closed}, Logw: logs})
	_, _, status := dialProxy(t, addr, http.MethodConnect, closed, closed)
	if !strings.HasPrefix(status, "HTTP/1.1 502") {
		t.Fatalf("status = %q, want 502", status)
	}
	if !strings.Contains(logs.String(), "allow "+closed) {
		t.Errorf("allow line missing before the dial failure: %q", logs.String())
	}
}

// TestConnectMalformedTarget pins the 400 branch for a CONNECT authority
// with no port.
func TestConnectMalformedTarget(t *testing.T) {
	addr := startProxy(t, &Proxy{Allow: []string{"api.anthropic.com"}})
	_, _, status := dialProxy(t, addr, http.MethodConnect, "portless.example.com", "portless.example.com")
	if !strings.HasPrefix(status, "HTTP/1.1 400") {
		t.Fatalf("status = %q, want 400", status)
	}
}

// TestHijackUnsupported drives serveHTTP against a plain ResponseRecorder
// (no Hijacker), which is the 500 branch a non-hijackable server would
// take. The dial target must be live so the failure is hijack, not dial.
func TestHijackUnsupported(t *testing.T) {
	echo := startEcho(t)
	target := echo.Addr().String()
	logs := &lockedBuf{}
	p := &Proxy{Allow: []string{target}, Logw: logs}
	req := httptest.NewRequest(http.MethodConnect, "http://"+target, nil)
	req.Host = target
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot hijack") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestSplitAllowEntry pins the entry grammar (port defaulting, whitespace).
func TestSplitAllowEntry(t *testing.T) {
	tests := []struct {
		in, host, port string
	}{
		{"api.anthropic.com", "api.anthropic.com", "443"},
		{"api.anthropic.com:8443", "api.anthropic.com", "8443"},
		{"  spaced.example.com  ", "spaced.example.com", "443"},
		{"127.0.0.1:9999", "127.0.0.1", "9999"},
	}
	for _, tc := range tests {
		h, p := splitAllowEntry(tc.in)
		if h != tc.host || p != tc.port {
			t.Errorf("splitAllowEntry(%q) = (%q, %q), want (%q, %q)", tc.in, h, p, tc.host, tc.port)
		}
	}
}

// hijackRecorder is a ResponseRecorder that implements http.Hijacker,
// handing serveHTTP either an error or a scripted client conn — the only
// way to reach the post-dial failure branches without a kernel that
// misbehaves on cue.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
	err  error
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h.err != nil {
		return nil, nil, h.err
	}
	rw := bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn))
	return h.conn, rw, nil
}

// brokenConn errors on first write — a client that vanished right after
// its CONNECT.
type brokenConn struct {
	net.Conn // nil; only the methods below are reachable in the test
}

func (brokenConn) Write([]byte) (int, error)   { return 0, fmt.Errorf("client gone") }
func (brokenConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (brokenConn) Close() error                { return nil }
func (brokenConn) SetDeadline(time.Time) error { return nil }

func TestConnectHijackFailurePaths(t *testing.T) {
	echo := startEcho(t)
	target := echo.Addr().String()
	logs := &lockedBuf{}
	p := &Proxy{Allow: []string{target}, Logw: logs}

	// CONNECT targets are authority-form ("host:port", no scheme) — that is
	// what a real client sends and what net/http parses into URL.Host.
	t.Run("hijack itself fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, target, nil)
		// Host header cleared: serveHTTP must fall back to the URL host.
		req.Host = ""
		rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder(), err: fmt.Errorf("no hijacking today")}
		p.Handler().ServeHTTP(rec, req)
		if !strings.Contains(logs.String(), "hijack failed") {
			t.Errorf("logs = %q", logs.String())
		}
	})
	t.Run("client write fails after hijack", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodConnect, target, nil)
		req.Host = target
		rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder(), conn: brokenConn{}}
		// Must return (closing both sides), not panic or hang.
		p.Handler().ServeHTTP(rec, req)
	})
}

// plainClient is a harness's HTTP client: HTTP_PROXY pointed at the proxy,
// so an http:// URL goes to it as an absolute-form request.
func plainClient(t *testing.T, proxyAddr string) *http.Client {
	t.Helper()
	pu, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: http.ProxyURL(pu)},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// A plain-http model endpoint the policy names is forwarded: the
// request reaches it with its body and key, the Host header is the target,
// hop-by-hop and proxy headers are dropped, and an event-stream answer
// arrives intact.
func TestPlainHTTPForwardedToAListedEndpoint(t *testing.T) {
	var got struct {
		host, auth, proxyAuth, conn, body string
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.host, got.auth, got.proxyAuth, got.conn = r.Host, r.Header.Get("Authorization"), r.Header.Get("Proxy-Authorization"), r.Header.Get("X-Hop")
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = fmt.Fprintf(w, "data: %d\n\n", i)
			fl.Flush()
		}
	}))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "http://")
	logs := &lockedBuf{}
	addr := startProxy(t, &Proxy{Allow: []string{target}, PlainHTTP: []string{target}, Logw: logs})
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "drop me")
	resp, err := plainClient(t, addr).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "data: 0\n\ndata: 1\n\ndata: 2\n\n" {
		t.Fatalf("status %d body %q, %v", resp.StatusCode, body, err)
	}
	if got.host != target || got.auth != "Bearer k" || got.body != `{"model":"m"}` {
		t.Fatalf("upstream saw host=%q auth=%q body=%q", got.host, got.auth, got.body)
	}
	if got.proxyAuth != "" || got.conn != "" {
		t.Fatalf("hop headers forwarded: Proxy-Authorization=%q X-Hop=%q", got.proxyAuth, got.conn)
	}
	if !strings.Contains(logs.String(), "allow http "+target) {
		t.Fatalf("no decision line: %q", logs.String())
	}
}

// A plain-http request to anything not on PlainHTTP is refused before a
// connection is made: a host on the CONNECT allowlist only, and an
// unlisted host. With PlainHTTP empty the proxy is CONNECT-only as before.
func TestPlainHTTPRefusedOffTheList(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "http://")
	logs := &lockedBuf{}
	addr := startProxy(t, &Proxy{Allow: []string{target, "llm.lan:8000"}, PlainHTTP: []string{"llm.lan:8000"}, Logw: logs})
	resp, err := plainClient(t, addr).Get(upstream.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || hit {
		t.Fatalf("status %d, upstream hit=%v", resp.StatusCode, hit)
	}
	if !strings.Contains(logs.String(), "deny http") {
		t.Fatalf("no deny line: %q", logs.String())
	}
	addr = startProxy(t, &Proxy{Allow: []string{target}})
	resp, err = plainClient(t, addr).Get(upstream.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || hit {
		t.Fatalf("CONNECT-only proxy: status %d, upstream hit=%v", resp.StatusCode, hit)
	}
}

// The proxy follows no redirect for a plain request: a 30x goes back to
// the harness as it came, so a key never rides to a Location.
func TestPlainHTTPFollowsNoRedirect(t *testing.T) {
	other := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other = true }))
	t.Cleanup(elsewhere.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "http://")
	addr := startProxy(t, &Proxy{Allow: []string{target}, PlainHTTP: []string{target}})
	resp, err := plainClient(t, addr).Get(upstream.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || other {
		t.Fatalf("status %d, redirect followed=%v", resp.StatusCode, other)
	}
}

// A PlainHTTP entry without a port means 80.
func TestPlainHTTPEntryDefaultsToPort80(t *testing.T) {
	if !permitsIn([]string{"llm.lan"}, "80", "llm.lan", "80") || permitsIn([]string{"llm.lan"}, "80", "llm.lan", "443") {
		t.Fatal("a portless PlainHTTP entry must mean :80 and only :80")
	}
}

// A streamed answer reaches the harness as it is written: the client reads
// the first event before the upstream will write the second.
func TestPlainHTTPStreamsAsItArrives(t *testing.T) {
	next := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: 0\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-next:
		case <-time.After(3 * time.Second):
			return
		}
		_, _ = fmt.Fprint(w, "data: 1\n\n")
	}))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "http://")
	addr := startProxy(t, &Proxy{Allow: []string{target}, PlainHTTP: []string{target}})
	resp, err := plainClient(t, addr).Get(upstream.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	if err != nil || first != "data: 0\n" {
		t.Fatalf("first event %q, %v: the proxy held the stream", first, err)
	}
	close(next)
	rest, _ := io.ReadAll(br)
	if string(rest) != "\ndata: 1\n\n" {
		t.Fatalf("rest = %q", rest)
	}
}

// An answer that starts before the request has been read to its end is
// relayed whole: the proxy must not close the request
// body under the upstream transport when it writes the answer's first
// bytes, or the transport, failing to read the rest, drops the upstream
// connection and cuts the answer after its first event. Here the client
// sends the rest of its body only after it has read the first event, so
// a proxy that is not full duplex never gets past it.
func TestPlainHTTPAnswerMayStartBeforeTheRequestEnds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("upstream full duplex: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: 0\n\n")
		w.(http.Flusher).Flush()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	}))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "http://")
	addr := startProxy(t, &Proxy{Allow: []string{target}, PlainHTTP: []string{target}})
	// The rest of the body goes once the first event is read, or after 3s,
	// so a proxy that cuts the answer fails the test rather than hang it.
	pr, pw := io.Pipe()
	firstRead := make(chan struct{})
	go func() {
		_, _ = io.WriteString(pw, `{"model":`)
		select {
		case <-firstRead:
		case <-time.After(3 * time.Second):
		}
		_, _ = io.WriteString(pw, `"m"}`)
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/chat/completions", pr)
	resp, err := plainClient(t, addr).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	first, err := br.ReadString('\n')
	close(firstRead)
	if err != nil || first != "data: 0\n" {
		t.Fatalf("first event %q, %v", first, err)
	}
	rest, err := io.ReadAll(br)
	if err != nil || string(rest) != "\ndata: {\"model\":\"m\"}\n\n" {
		t.Fatalf("rest = %q, %v: the answer was cut", rest, err)
	}
}

// The upstream is the URL's host:port, whatever the Host header says: a
// request cannot pass the check under one name and reach another.
func TestPlainHTTPGoesToTheURLNotTheHostHeader(t *testing.T) {
	var sawHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sawHost = r.Host }))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "http://")
	addr := startProxy(t, &Proxy{Allow: []string{target}, PlainHTTP: []string{target}})
	_, _, status := dialProxy(t, addr, http.MethodGet, upstream.URL+"/v1/models", "metadata.internal")
	if !strings.HasPrefix(status, "HTTP/1.1 200") || sawHost != target {
		t.Fatalf("status %q, upstream saw Host %q, want %q", status, sawHost, target)
	}
	// And origin-form with a listed Host is not a forward request at all.
	_, _, status = dialProxy(t, addr, http.MethodGet, "/v1/models", target)
	if !strings.HasPrefix(status, "HTTP/1.1 405") {
		t.Fatalf("origin-form request: status %q, want 405", status)
	}
}

// A PlainHTTP entry the CONNECT allowlist does not name is not forwarded:
// the proxy holds the subset rule itself, whoever built its flags.
func TestPlainHTTPMustAlsoBeOnTheAllowlist(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	t.Cleanup(upstream.Close)
	target := strings.TrimPrefix(upstream.URL, "http://")
	addr := startProxy(t, &Proxy{Allow: []string{"api.anthropic.com"}, PlainHTTP: []string{target}})
	resp, err := plainClient(t, addr).Get(upstream.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || hit {
		t.Fatalf("status %d, upstream hit=%v", resp.StatusCode, hit)
	}
}

// fakeResolver answers every lookup with its addresses.
type fakeResolver []string

func (f fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	var out []net.IPAddr
	for _, a := range f {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	return out, nil
}

// TestPublicOnlyRefusesAPrivateAnswer: an input source's proxy refuses a
// permitted host that resolves to any address off the public internet,
// even when another of its answers is public.
func TestPublicOnlyRefusesAPrivateAnswer(t *testing.T) {
	for _, answer := range [][]string{{"127.0.0.1"}, {"10.1.2.3"}, {"169.254.169.254"}, {"100.101.102.103"}, {"8.8.8.8", "192.168.1.1"}, {"::1"}, {"fd00::1"}, {"fe80::1"}, {"::ffff:127.0.0.1"}} {
		logs := &lockedBuf{}
		p := &Proxy{Allow: []string{"source.example:443"}, PublicOnly: true, Resolver: fakeResolver(answer), Logw: logs}
		srv := httptest.NewServer(p.Handler())
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintf(conn, "CONNECT source.example:443 HTTP/1.1\r\nHost: source.example:443\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%v: status %d, want 403", answer, resp.StatusCode)
		}
		if !strings.Contains(logs.String(), "not a public address") {
			t.Errorf("%v: the denial does not say why: %s", answer, logs.String())
		}
		_ = conn.Close()
		srv.Close()
	}
	// An IP literal is held to the same rule.
	if Public(net.ParseIP("127.0.0.1")) || !Public(net.ParseIP("140.82.112.3")) || !Public(net.ParseIP("2606:4700::1")) {
		t.Error("Public misjudges a literal")
	}
}

// TestPublicOnlyForwardsNoPlainHTTP: a source's proxy tunnels and nothing
// else, even were a plain-http endpoint named.
func TestPublicOnlyForwardsNoPlainHTTP(t *testing.T) {
	p := &Proxy{Allow: []string{"source.example:80"}, PlainHTTP: []string{"source.example:80"}, PublicOnly: true, Resolver: fakeResolver{"8.8.8.8"}}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	resp, err := c.Get("http://source.example/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", resp.StatusCode)
	}
}

// connectStatus sends one CONNECT to the proxy and answers the status code and
// the proxy's log.
func connectStatus(t *testing.T, p *Proxy, target string) int {
	t.Helper()
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// TestNetworkOKRefusesWhatIsNotANetworkAddress: a delivery's proxy for a
// destination on the person's network tunnels to a private, shared or
// loopback answer but never to link-local space (where cloud metadata
// answers), and a name that resolves to one such address among others is
// refused whole.
func TestNetworkOKRefusesWhatIsNotANetworkAddress(t *testing.T) {
	for _, answer := range [][]string{{"169.254.169.254"}, {"fe80::1"}, {"8.8.8.8", "169.254.169.254"}, {"0.0.0.0"}, {"224.0.0.1"}, {"::ffff:169.254.169.254"}} {
		logs := &lockedBuf{}
		p := &Proxy{Allow: []string{"hook.example:443"}, NetworkOK: true, Resolver: fakeResolver(answer), Logw: logs}
		if got := connectStatus(t, p, "hook.example:443"); got != http.StatusForbidden {
			t.Errorf("%v: status %d, want 403", answer, got)
		}
		if !strings.Contains(logs.String(), "private-network address") {
			t.Errorf("%v: the denial does not say why: %s", answer, logs.String())
		}
	}
	echo := startEcho(t)
	host, port, _ := net.SplitHostPort(echo.Addr().String())
	for _, answer := range [][]string{{"127.0.0.1"}, {"10.1.2.3", "127.0.0.1"}} {
		var dialled string
		p := &Proxy{Allow: []string{"lan.example:" + port}, NetworkOK: true, Resolver: fakeResolver(answer), Logw: &lockedBuf{},
			dial: func(ctx context.Context, address string) (net.Conn, error) {
				dialled = address
				return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
			}}
		if got := connectStatus(t, p, "lan.example:"+port); got != http.StatusOK {
			t.Errorf("%v: status %d, want 200", answer, got)
		}
		// The address checked is the one dialled, never the name again.
		if dialled != net.JoinHostPort(answer[0], port) {
			t.Errorf("%v: dialled %q", answer, dialled)
		}
	}
	// A host not on the list is refused whatever it resolves to.
	if got := connectStatus(t, &Proxy{Allow: []string{"lan.example:443"}, NetworkOK: true, Resolver: fakeResolver{"10.0.0.1"}}, "other.example:443"); got != http.StatusForbidden {
		t.Errorf("an unlisted host: status %d, want 403", got)
	}
}

// TestNetworkOKForwardsNoPlainHTTP: like a source's proxy, a delivery's
// tunnels and forwards no plain request, so the address check is never
// bypassed.
func TestNetworkOKForwardsNoPlainHTTP(t *testing.T) {
	p := &Proxy{Allow: []string{"lan.example:80"}, PlainHTTP: []string{"lan.example:80"}, NetworkOK: true, Resolver: fakeResolver{"10.0.0.1"}}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	resp, err := c.Get("http://lan.example/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", resp.StatusCode)
	}
}

// noSuchHost stands in for a dial whose name does not resolve.
func noSuchHost(_ context.Context, address string) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(address)
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
}

// A tunnel to a host the machine that started the proxy could not resolve
// either is refused with a sentence the client reports, since a client
// sees a refused tunnel's status line and nothing else.
func TestConnectToAnUnresolvedHostSaysSoPlainly(t *testing.T) {
	logs := &lockedBuf{}
	addr := startProxy(t, &Proxy{Allow: []string{"gone.invalid:443", "other.invalid:443"}, Unresolved: []string{"gone.invalid"}, Logw: logs, dial: noSuchHost})
	_, err := plainClient(t, addr).Get("https://GONE.invalid/v1/models")
	want := "the model endpoint's host gone.invalid does not resolve: check the endpoint's address in the model settings"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to say %q", err, want)
	}
	if !strings.Contains(logs.String(), "unresolved GONE.invalid:443") {
		t.Errorf("no decision line: %q", logs.String())
	}
	// A host the machine resolved gets the plain 502.
	_, _, status := dialProxy(t, addr, http.MethodConnect, "other.invalid:443", "other.invalid:443")
	if status != "HTTP/1.1 502 Bad Gateway" {
		t.Errorf("status = %q, want the plain 502", status)
	}
}

// A plain-http request to an unresolved host gets the same sentence in the
// 502's body, which the harness quotes.
func TestPlainHTTPToAnUnresolvedHostSaysSoPlainly(t *testing.T) {
	target := "gone.invalid:8000"
	addr := startProxy(t, &Proxy{Allow: []string{target}, PlainHTTP: []string{target}, Unresolved: []string{"gone.invalid"}})
	resp, err := plainClient(t, addr).Get("http://" + target + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "gone.invalid does not resolve: check") {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

// Any other dial failure of an unresolved host keeps the dial's own error.
func TestUnresolvedMessageOnlyForALookupFailure(t *testing.T) {
	p := &Proxy{Unresolved: []string{"gone.invalid"}}
	if got := p.unresolvedMessage("gone.invalid", &net.OpError{Op: "dial", Err: fmt.Errorf("connection refused")}); got != "" {
		t.Errorf("a refused connection: %q", got)
	}
	if got := p.unresolvedMessage("elsewhere.invalid", &net.DNSError{IsNotFound: true}); got != "" {
		t.Errorf("a host not on the list: %q", got)
	}
}

// The status line written by hand carries no control character, whatever
// the allowlist entry held.
func TestFailConnectDropsControlCharacters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failConnect(w, "gone.invalid\r\nX-Injected: 1")
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", addr)
	raw, _ := io.ReadAll(c)
	status, headers, _ := strings.Cut(string(raw), "\r\n")
	if status != "HTTP/1.1 502 gone.invalidX-Injected: 1" {
		t.Errorf("status line = %q", status)
	}
	if strings.Contains(headers, "\r\nX-Injected") || strings.HasPrefix(headers, "X-Injected") {
		t.Errorf("a header was injected: %q", raw)
	}
}

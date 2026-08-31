// End-to-end tests of the CONNECT allowlist: a permitted tunnel must
// carry bytes both ways, a denied one must die with a 403 and a receipt
// line, and nothing but CONNECT may pass — the proxy is the single hole
// in the agent container's network isolation (RUNNER-PLAN §R4), so its
// behavior is pinned at the socket level, not through a mock.

package netproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	echo := startEcho(t)
	target := echo.Addr().String()
	logs := &lockedBuf{}
	addr := startProxy(t, &Proxy{Allow: []string{target}, Logw: logs})

	c, br, status := dialProxy(t, addr, http.MethodConnect, target, target)
	if status != "HTTP/1.1 200 Connection Established" {
		t.Fatalf("status = %q", status)
	}
	// Bytes must round-trip through the tunnel in both directions.
	if _, err := c.Write([]byte("ping through tunnel\n")); err != nil {
		t.Fatal(err)
	}
	line, err := br.ReadString('\n')
	if err != nil || line != "ping through tunnel\n" {
		t.Fatalf("echoed line = %q, err %v", line, err)
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
	echo := startEcho(t)
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
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := br.ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("echo through ListenAndServe tunnel = (%q, %v)", line, err)
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

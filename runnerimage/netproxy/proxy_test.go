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

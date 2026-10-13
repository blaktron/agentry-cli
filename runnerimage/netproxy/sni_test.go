// The SNI check (agentry-cli#616): a tunnel the allowlist opened to one
// host carries only a ClientHello that names that host, so a CDN front end
// serving the host cannot be asked for another site. Pinned end to end
// with Go's own TLS client, and with hand-built hellos for what Go's
// client never sends.

package netproxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
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

// startTLSEcho runs a TLS upstream that echoes each line it reads, with
// httptest's certificate: the stand-in for a model endpoint.
func startTLSEcho(t *testing.T) net.Listener {
	t.Helper()
	cert := httptest.NewUnstartedServer(nil)
	cert.StartTLS()
	cfg := cert.TLS.Clone()
	cert.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
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

// recordingUpstream is a raw TCP upstream that keeps every byte it is
// sent, to prove a refused tunnel sent it none.
type recordingUpstream struct {
	net.Listener
	mu  sync.Mutex
	got bytes.Buffer
}

func (u *recordingUpstream) bytes() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.got.Bytes()...)
}

func startRecording(t *testing.T) *recordingUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &recordingUpstream{Listener: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					u.mu.Lock()
					u.got.Write(buf[:n])
					u.mu.Unlock()
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return u
}

// tunnelConn is the client's side of an established tunnel: reads go
// through the reader that consumed the CONNECT response.
type tunnelConn struct {
	net.Conn
	r *bufio.Reader
}

func (c tunnelConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// openTunnel CONNECTs to target through the proxy and fails the test
// unless the proxy answers 200.
func openTunnel(t *testing.T, proxyAddr, target string) tunnelConn {
	t.Helper()
	c, br, status := dialProxy(t, proxyAddr, http.MethodConnect, target, target)
	if status != "HTTP/1.1 200 Connection Established" {
		t.Fatalf("CONNECT %s: status = %q", target, status)
	}
	return tunnelConn{Conn: c, r: br}
}

// tlsThrough runs a TLS handshake with serverName over the tunnel and,
// when it succeeds, round-trips one line.
func tlsThrough(conn net.Conn, serverName string) error {
	tc := tls.Client(conn, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}) //nolint:gosec // the test's own certificate
	if err := tc.Handshake(); err != nil {
		return err
	}
	if _, err := tc.Write([]byte("ping through tls\n")); err != nil {
		return err
	}
	line, err := bufio.NewReader(tc).ReadString('\n')
	if err != nil {
		return err
	}
	if line != "ping through tls\n" {
		return fmt.Errorf("echo = %q", line)
	}
	return nil
}

func TestSNIMatchingTheHostPasses(t *testing.T) {
	echo := startTLSEcho(t)
	_, port, _ := net.SplitHostPort(echo.Addr().String())
	for _, target := range []string{"localhost:" + port, "LocalHost:" + port} {
		logs := &lockedBuf{}
		addr := startProxy(t, &Proxy{Allow: []string{"localhost:" + port}, Logw: logs})
		conn := openTunnel(t, addr, target)
		// The name compares case-insensitively, as the allowlist does.
		if err := tlsThrough(conn, "localhost"); err != nil {
			t.Fatalf("%s: TLS through the tunnel: %v (log %q)", target, err, logs.String())
		}
		if strings.Contains(logs.String(), "deny") {
			t.Errorf("%s: a matching hello was logged as a denial: %q", target, logs.String())
		}
	}
}

// An address literal carries no server name (Go's client sends none for
// an IP), and that is allowed for a tunnel to the address.
func TestSNIAbsentForAnAddressLiteralPasses(t *testing.T) {
	echo := startTLSEcho(t)
	target := echo.Addr().String()
	addr := startProxy(t, &Proxy{Allow: []string{target}})
	if err := tlsThrough(openTunnel(t, addr, target), "127.0.0.1"); err != nil {
		t.Fatalf("TLS to an address literal: %v", err)
	}
}

func TestSNIRefusedBeforeAnyUpstreamByte(t *testing.T) {
	up := startRecording(t)
	_, port, _ := net.SplitHostPort(up.Addr().String())
	host := "localhost:" + port
	ip := up.Addr().String()

	cases := []struct {
		name, target string
		send         func(net.Conn)
		wantLog      string
	}{
		{"another host's name", host, func(c net.Conn) {
			_ = tls.Client(c, &tls.Config{ServerName: "www.google.com", InsecureSkipVerify: true}).Handshake()
		}, `deny sni "www.google.com" for ` + host}, //nolint:gosec
		{"no name", host, func(c net.Conn) { _, _ = c.Write(goHello(t, "")) }, `deny sni "" for ` + host + " (it names no server)"}, //nolint:gosec
		{"a name on a tunnel to an address", ip, func(c net.Conn) {
			_ = tls.Client(c, &tls.Config{ServerName: "www.google.com", InsecureSkipVerify: true}).Handshake()
		}, `deny sni "www.google.com" for ` + ip}, //nolint:gosec
		{"not TLS", host, func(c net.Conn) { _, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: www.google.com\r\n\r\n")) }, "deny sni for " + host + " (not a TLS ClientHello: record type 71)"},
		{"an alert first", host, func(c net.Conn) { _, _ = c.Write([]byte{21, 3, 3, 0, 2, 2, 40}) }, "not a TLS ClientHello: record type 21"},
		{"an encrypted hello", host, func(c net.Conn) { _, _ = c.Write(helloRecords(buildHello("localhost", extECH), 1<<14)) }, "offers an encrypted ClientHello"},
		{"the draft encrypted name", host, func(c net.Conn) { _, _ = c.Write(helloRecords(buildHello("localhost", extESNI), 1<<14)) }, "offers an encrypted ClientHello"},
		{"two host names", host, func(c net.Conn) { _, _ = c.Write(helloRecords(buildHello("localhost\x00www.google.com"), 1<<14)) }, "more than one host"},
		{"an oversized hello", host, func(c net.Conn) { _, _ = c.Write([]byte{22, 3, 1, 0, 4, 1, 0, 0x80, 0}) }, "over the proxy's"},
		{"an oversized record", host, func(c net.Conn) { _, _ = c.Write([]byte{22, 3, 1, 0x40, 1}) }, "record length 16385"},
		{"a long-s name", host, func(c net.Conn) { _, _ = c.Write(helloRecords(buildHello("locaſhost"), 1<<14)) }, "names another host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuf{}
			addr := startProxy(t, &Proxy{Allow: []string{host, ip}, Logw: logs})
			conn := openTunnel(t, addr, tc.target)
			before := len(up.bytes())
			tc.send(conn)
			waitClosed(t, conn)
			if !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("log = %q, want it to hold %q", logs.String(), tc.wantLog)
			}
			if got := up.bytes()[before:]; len(got) != 0 {
				t.Errorf("the upstream was sent %d bytes", len(got))
			}
		})
	}
}

// waitClosed reads the client's side until the proxy closes it.
func waitClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.Copy(io.Discard, conn)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the proxy kept a refused tunnel open")
	}
}

func TestSNISlowHelloRefused(t *testing.T) {
	old := helloTimeout
	helloTimeout = 200 * time.Millisecond
	t.Cleanup(func() { helloTimeout = old })
	up := startRecording(t)
	_, port, _ := net.SplitHostPort(up.Addr().String())
	host := "localhost:" + port
	logs := &lockedBuf{}
	addr := startProxy(t, &Proxy{Allow: []string{host}, Logw: logs})
	conn := openTunnel(t, addr, host)
	_, _ = conn.Write([]byte{22, 3, 1}) // and nothing more
	waitClosed(t, conn)
	if !strings.Contains(logs.String(), "deny sni for "+host) || !strings.Contains(logs.String(), "timeout") {
		t.Errorf("log = %q", logs.String())
	}
	if len(up.bytes()) != 0 {
		t.Error("a slow hello reached the upstream")
	}
}

// A hello fragmented over several records, pipelined behind the CONNECT
// itself, is read whole and forwarded byte for byte.
func TestSNIFragmentedPipelinedHelloForwardedIntact(t *testing.T) {
	up := startRecording(t)
	_, port, _ := net.SplitHostPort(up.Addr().String())
	host := "localhost:" + port
	addr := startProxy(t, &Proxy{Allow: []string{host}})
	wire := helloRecords(buildHello("localhost"), 7)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n%s", host, host, wire); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !bytes.Equal(up.bytes(), wire) {
		if time.Now().After(deadline) {
			t.Fatalf("upstream got %d bytes, want the %d of the hello", len(up.bytes()), len(wire))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A CONNECT to a plain-http model endpoint (DR-45) is the one tunnel that
// carries no TLS, and is relayed as before.
func TestSNINotAskedOfAPlainHTTPTarget(t *testing.T) {
	echo := startEcho(t)
	target := echo.Addr().String()
	addr := startProxy(t, &Proxy{Allow: []string{target}, PlainHTTP: []string{target}})
	conn := openTunnel(t, addr, target)
	if _, err := conn.Write([]byte("plain\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := conn.r.ReadString('\n'); err != nil || line != "plain\n" {
		t.Fatalf("echo = (%q, %v)", line, err)
	}
}

// goHello is the ClientHello Go's TLS client sends for serverName, as it
// crosses the wire.
func goHello(t *testing.T, serverName string) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	go func() {
		_ = tls.Client(a, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake() //nolint:gosec // never completes
	}()
	raw, _, err := readClientHello(b)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReadClientHelloFromGo(t *testing.T) {
	for _, name := range []string{"api.osv.dev", ""} {
		a, b := net.Pipe()
		go func() {
			_ = tls.Client(a, &tls.Config{ServerName: name, InsecureSkipVerify: true}).Handshake() //nolint:gosec
		}()
		_, h, err := readClientHello(b)
		_ = a.Close()
		_ = b.Close()
		if err != nil || h.serverName != name || h.encrypted {
			t.Errorf("ServerName %q: hello = %+v, err %v", name, h, err)
		}
	}
}

func TestJudgeHello(t *testing.T) {
	cases := []struct {
		h    clientHello
		host string
		ok   bool
	}{
		{clientHello{serverName: "api.osv.dev"}, "api.osv.dev", true},
		{clientHello{serverName: "API.OSV.DEV"}, "api.osv.dev", true},
		{clientHello{serverName: "api.oſv.dev"}, "api.osv.dev", false},
		{clientHello{serverName: "www.google.com"}, "api.osv.dev", false},
		{clientHello{serverName: "osv.dev"}, "api.osv.dev", false},
		{clientHello{serverName: "api.osv.dev"}, "api.osv.dev.", true}, // a client sends the name without its dot
		{clientHello{serverName: "api.osv.dev."}, "api.osv.dev", true},
		{clientHello{serverName: "api.osv.dev.."}, "api.osv.dev", false},
		{clientHello{}, "api.osv.dev", false},
		{clientHello{}, "203.0.113.7", true},
		{clientHello{}, "2001:db8::1", true},
		{clientHello{serverName: "203.0.113.7"}, "203.0.113.7", true},
		{clientHello{serverName: "api.osv.dev"}, "203.0.113.7", false},
		{clientHello{serverName: "api.osv.dev", encrypted: true}, "api.osv.dev", false},
	}
	for _, tc := range cases {
		if got := judgeHello(tc.h, tc.host) == ""; got != tc.ok {
			t.Errorf("judgeHello(%+v, %q) allows = %v, want %v", tc.h, tc.host, got, tc.ok)
		}
	}
}

func TestPermitsIsASCIIOnly(t *testing.T) {
	p := &Proxy{Allow: []string{"api.osv.dev"}}
	if p.permits("api.oſv.dev", "443") {
		t.Error(`"api.oſv.dev" (U+017F) matched "api.osv.dev"`)
	}
	if !p.permits("API.osv.DEV", "443") {
		t.Error("ASCII case no longer folds")
	}
}

// buildHello is a minimal TLS 1.2-shaped ClientHello body with its
// handshake header: one host_name per NUL-separated name (none for ""),
// then an empty extension of each extra type.
func buildHello(names string, extra ...uint16) []byte {
	var ext bytes.Buffer
	if names != "" {
		var list bytes.Buffer
		for _, n := range strings.Split(names, "\x00") {
			list.WriteByte(serverNameHostName)
			_ = binary.Write(&list, binary.BigEndian, uint16(len(n)))
			list.WriteString(n)
		}
		_ = binary.Write(&ext, binary.BigEndian, uint16(extServerName))
		_ = binary.Write(&ext, binary.BigEndian, uint16(list.Len()+2))
		_ = binary.Write(&ext, binary.BigEndian, uint16(list.Len()))
		ext.Write(list.Bytes())
	}
	for _, typ := range extra {
		_ = binary.Write(&ext, binary.BigEndian, typ)
		_ = binary.Write(&ext, binary.BigEndian, uint16(0))
	}
	var body bytes.Buffer
	body.Write([]byte{3, 3})
	body.Write(make([]byte, 32)) // random
	body.WriteByte(0)            // session id
	body.Write([]byte{0, 2, 0x13, 0x01})
	body.Write([]byte{1, 0}) // compression: null
	_ = binary.Write(&body, binary.BigEndian, uint16(ext.Len()))
	body.Write(ext.Bytes())
	n := body.Len()
	return append([]byte{handshakeHello, byte(n >> 16), byte(n >> 8), byte(n)}, body.Bytes()...)
}

// helloRecords frames a handshake message as records of at most size
// bytes each.
func helloRecords(msg []byte, size int) []byte {
	var out bytes.Buffer
	for len(msg) > 0 {
		n := min(size, len(msg))
		out.Write([]byte{recordHandshake, 3, 1, byte(n >> 8), byte(n)})
		out.Write(msg[:n])
		msg = msg[n:]
	}
	return out.Bytes()
}

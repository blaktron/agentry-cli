package messenger

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455's handshake names SHA-1; it proves nothing secret
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A WebSocket client (RFC 6455), the client side alone: the opening
// handshake over TLS through the relay's CONNECT, text messages read and
// written whole, pings answered, and the closing handshake. No extension
// is offered, so no frame is ever compressed. Discord's gateway and Slack's
// Socket Mode both run over it.

// wsGUID is RFC 6455's key suffix for Sec-WebSocket-Accept.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxWSMessage bounds one message the role reads.
const maxWSMessage = 4 << 20

// wsWriteTimeout bounds one frame's write.
const wsWriteTimeout = 30 * time.Second

// The opcodes.
const (
	wsContinuation = 0x0
	wsText         = 0x1
	wsBinary       = 0x2
	wsClose        = 0x8
	wsPing         = 0x9
	wsPong         = 0xA
)

// wsConn is one open WebSocket.
type wsConn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
}

// wsCloseError is the peer's close: its code and reason.
type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("the WebSocket was closed (%d)", e.Code)
	}
	return fmt.Sprintf("the WebSocket was closed (%d): %s", e.Code, e.Reason)
}

// dialWebSocket opens rawURL (wss://, or ws:// for a test's server) through
// proxy, an http:// CONNECT proxy or "" for none.
func dialWebSocket(ctx context.Context, rawURL, proxy string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "wss" && u.Scheme != "ws") {
		return nil, errors.New("the WebSocket's address is not wss://")
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "ws" {
			port = "80"
		}
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialThrough(ctx, addr, proxy)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	if d, has := ctx.Deadline(); has {
		_ = conn.SetDeadline(d)
	}
	if u.Scheme == "wss" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("the WebSocket's TLS handshake: %w", err)
		}
		conn = tc
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	k := base64.StdEncoding.EncodeToString(key)
	req := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: u.EscapedPath(), RawQuery: u.RawQuery}, Host: u.Host, Header: http.Header{}, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
	if req.URL.Path == "" {
		req.URL.Path = "/"
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", k)
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("User-Agent", "agentry-messenger")
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("the WebSocket's handshake: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("the WebSocket's handshake: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("the WebSocket's handshake was answered HTTP %d", resp.StatusCode)
	}
	sum := sha1.Sum([]byte(k + wsGUID)) //nolint:gosec // as above
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(resp.Header.Get("Connection")), "upgrade") ||
		resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		return nil, errors.New("the WebSocket's handshake was not answered as RFC 6455's")
	}
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		return nil, errors.New("the server chose a WebSocket extension the role did not offer")
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return &wsConn{c: conn, br: br}, nil
}

// dialThrough opens a TCP connection to addr, through proxy's CONNECT when
// there is one.
func dialThrough(ctx context.Context, addr, proxy string) (net.Conn, error) {
	var d net.Dialer
	if proxy == "" {
		return d.DialContext(ctx, "tcp", addr)
	}
	pu, err := url.Parse(proxy)
	if err != nil || pu.Scheme != "http" || pu.Host == "" {
		return nil, fmt.Errorf("the relay's address %q is not an http:// proxy", proxy)
	}
	conn, err := d.DialContext(ctx, "tcp", pu.Host)
	if err != nil {
		return nil, fmt.Errorf("the relay: %w", err)
	}
	if dl, has := ctx.Deadline(); has {
		_ = conn.SetDeadline(dl)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the relay: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("the relay: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("the relay refused %s (HTTP %d)", addr, resp.StatusCode)
	}
	if br.Buffered() > 0 {
		// Nothing may follow the proxy's answer before the TLS handshake.
		_ = conn.Close()
		return nil, errors.New("the relay sent more than its answer")
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// read is the next text or binary message, answering pings on the way. A
// connection quiet for idle, with not even a pong, is given up on: every
// frame that arrives moves the deadline.
func (w *wsConn) read(idle time.Duration) ([]byte, error) {
	var msg []byte
	started := false
	for {
		_ = w.c.SetReadDeadline(time.Now().Add(idle))
		fin, op, payload, err := w.frame()
		if err != nil {
			return nil, err
		}
		switch op {
		case wsPing:
			if err := w.write(wsPong, payload); err != nil {
				return nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			ce := &wsCloseError{Code: 1005}
			if len(payload) >= 2 {
				ce.Code = int(binary.BigEndian.Uint16(payload))
				ce.Reason = strings.ToValidUTF8(string(payload[2:]), "")
			}
			_ = w.write(wsClose, payload[:min(len(payload), 2)])
			_ = w.c.Close()
			return nil, ce
		case wsText, wsBinary:
			if started {
				return nil, errors.New("a WebSocket message began inside another")
			}
			started = true
		case wsContinuation:
			if !started {
				return nil, errors.New("a WebSocket continuation began no message")
			}
		default:
			return nil, fmt.Errorf("a WebSocket frame of an unknown kind (%d)", op)
		}
		if len(msg)+len(payload) > maxWSMessage {
			return nil, fmt.Errorf("a WebSocket message is longer than %d bytes", maxWSMessage)
		}
		msg = append(msg, payload...)
		if fin {
			return msg, nil
		}
	}
}

// frame reads one frame.
func (w *wsConn) frame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err := io.ReadFull(w.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0f
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("a WebSocket frame set a reserved bit, which no extension the role offered sets")
	}
	if h[1]&0x80 != 0 {
		return false, 0, nil, errors.New("the server masked a WebSocket frame, which RFC 6455 forbids")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(w.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(w.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= wsClose && (n > 125 || !fin) {
		return false, 0, nil, errors.New("a WebSocket control frame was fragmented or longer than 125 bytes")
	}
	if n > maxWSMessage {
		return false, 0, nil, fmt.Errorf("a WebSocket frame is longer than %d bytes", maxWSMessage)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, op, payload, nil
}

// writeText sends one text message.
func (w *wsConn) writeText(b []byte) error { return w.write(wsText, b) }

// ping sends a ping, whose pong the next read takes.
func (w *wsConn) ping() error { return w.write(wsPing, nil) }

// write sends one frame, masked as a client's must be.
func (w *wsConn) write(op byte, payload []byte) error {
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	b := make([]byte, 0, 14+len(payload))
	b = append(b, 0x80|op)
	switch n := len(payload); {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n <= 0xffff:
		b = append(b, 0x80|126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	b = append(b, mask[:]...)
	for i, c := range payload {
		b = append(b, c^mask[i%4])
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	_, err := w.c.Write(b)
	return err
}

// close sends a normal close and closes the connection.
func (w *wsConn) close() {
	_ = w.write(wsClose, []byte{0x03, 0xe8})
	_ = w.c.Close()
}

package netproxy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The SNI check (agentry-cli#616, DR-6 as amended). A CDN front end routes
// a TLS connection by the server name in its ClientHello, not by the
// address it arrived at, so a tunnel the allowlist opened to one host could
// otherwise reach every site that front end serves. The proxy therefore
// reads the client's ClientHello before it forwards a byte, requires its
// server_name to be the host the tunnel was opened to, and then relays the
// hello and everything after it unchanged. It reads the name only: there
// is still no decryption and no certificate of the proxy's own.

const (
	// maxHelloBytes bounds the ClientHello's handshake message. A real one
	// is a few hundred bytes, under 2 KiB with a post-quantum key share.
	maxHelloBytes = 32 << 10
	// maxRecordBytes is TLS's limit on a plaintext record (RFC 8446 §5.1).
	maxRecordBytes = 1 << 14
)

// helloTimeout bounds how long a client may take to send its hello once
// the tunnel is established, so a slow client cannot hold one. A variable
// only so a test need not wait it out.
var helloTimeout = 10 * time.Second

const (
	recordHandshake    = 22
	handshakeHello     = 1
	extServerName      = 0x0000
	extECH             = 0xfe0d // encrypted_client_hello
	extESNI            = 0xffce // the draft encrypted_server_name
	serverNameHostName = 0
)

// clientHello is what the check needs from a ClientHello.
type clientHello struct {
	serverName string // "" when the hello names no host
	encrypted  bool   // the hello offers ECH or ESNI
}

// errNotHello marks bytes that are not a TLS ClientHello at all.
var errNotHello = errors.New("not a TLS ClientHello")

// readClientHello reads the client's first handshake message from r,
// across as many handshake records as it spans, and returns every byte it
// read (so the tunnel can forward them as they came) with the parsed hello.
func readClientHello(r io.Reader) ([]byte, clientHello, error) {
	var raw, msg []byte
	need := -1 // the handshake message's full length, once its header is in
	for need < 0 || len(msg) < need {
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return raw, clientHello{}, fmt.Errorf("read the TLS record header: %w", err)
		}
		raw = append(raw, hdr[:]...)
		if hdr[0] != recordHandshake {
			return raw, clientHello{}, fmt.Errorf("%w: record type %d", errNotHello, hdr[0])
		}
		if hdr[1] != 3 {
			return raw, clientHello{}, fmt.Errorf("%w: record version %#04x", errNotHello, binary.BigEndian.Uint16(hdr[1:3]))
		}
		n := int(binary.BigEndian.Uint16(hdr[3:5]))
		if n == 0 || n > maxRecordBytes {
			return raw, clientHello{}, fmt.Errorf("%w: record length %d", errNotHello, n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return raw, clientHello{}, fmt.Errorf("read a TLS record: %w", err)
		}
		raw = append(raw, body...)
		msg = append(msg, body...)
		if need < 0 && len(msg) >= 4 {
			if msg[0] != handshakeHello {
				return raw, clientHello{}, fmt.Errorf("%w: handshake type %d", errNotHello, msg[0])
			}
			need = 4 + (int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3]))
			if need > maxHelloBytes {
				return raw, clientHello{}, fmt.Errorf("the ClientHello is %d bytes, over the proxy's %d", need, maxHelloBytes)
			}
		}
		if need >= 0 && len(msg) > need {
			// A second handshake message behind the hello, before the
			// server has answered: no client does that.
			return raw, clientHello{}, fmt.Errorf("%w: bytes after the ClientHello", errNotHello)
		}
	}
	h, err := parseClientHello(msg[4:need])
	return raw, h, err
}

// parseClientHello reads the server_name and the encrypted-hello
// extensions from a ClientHello's body (RFC 8446 §4.1.2, RFC 6066 §3).
func parseClientHello(b []byte) (clientHello, error) {
	var h clientHello
	s := helloReader(b)
	if !s.skip(2+32) || !s.skipVec(1) || !s.skipVec(2) || !s.skipVec(1) {
		return h, fmt.Errorf("%w: truncated", errNotHello)
	}
	if len(s) == 0 {
		return h, nil // no extensions at all, so no server name
	}
	exts, ok := s.vec(2)
	if !ok || len(s) != 0 {
		return h, fmt.Errorf("%w: malformed extensions", errNotHello)
	}
	seen := map[uint16]bool{}
	for len(exts) > 0 {
		typ, ok := exts.u16()
		if !ok {
			return h, fmt.Errorf("%w: malformed extension", errNotHello)
		}
		data, ok := exts.vec(2)
		if !ok {
			return h, fmt.Errorf("%w: malformed extension %#04x", errNotHello, typ)
		}
		if seen[typ] {
			return h, fmt.Errorf("%w: extension %#04x twice", errNotHello, typ)
		}
		seen[typ] = true
		switch typ {
		case extECH, extESNI:
			h.encrypted = true
		case extServerName:
			name, err := parseServerName(data)
			if err != nil {
				return h, err
			}
			h.serverName = name
		}
	}
	return h, nil
}

// parseServerName answers the one host_name of a server_name extension.
func parseServerName(data helloReader) (string, error) {
	list, ok := data.vec(2)
	if !ok || len(data) != 0 {
		return "", fmt.Errorf("%w: malformed server_name", errNotHello)
	}
	name := ""
	for len(list) > 0 {
		typ, ok := list.u8()
		if !ok {
			return "", fmt.Errorf("%w: malformed server_name", errNotHello)
		}
		v, ok := list.vec(2)
		if !ok {
			return "", fmt.Errorf("%w: malformed server_name", errNotHello)
		}
		if typ != serverNameHostName {
			continue
		}
		if name != "" || len(v) == 0 {
			// RFC 6066 §3 allows one name of a type; two would let the
			// check read one and the front end route by the other.
			return "", fmt.Errorf("%w: server_name names more than one host", errNotHello)
		}
		name = string(v)
	}
	return name, nil
}

// helloReader is a cursor over a hello's bytes.
type helloReader []byte

func (s *helloReader) skip(n int) bool {
	if len(*s) < n {
		return false
	}
	*s = (*s)[n:]
	return true
}

func (s *helloReader) u8() (byte, bool) {
	if len(*s) < 1 {
		return 0, false
	}
	v := (*s)[0]
	*s = (*s)[1:]
	return v, true
}

func (s *helloReader) u16() (uint16, bool) {
	if len(*s) < 2 {
		return 0, false
	}
	v := binary.BigEndian.Uint16(*s)
	*s = (*s)[2:]
	return v, true
}

// vec reads a vector with a 1- or 2-byte length prefix.
func (s *helloReader) vec(lenBytes int) (helloReader, bool) {
	var n int
	switch lenBytes {
	case 1:
		v, ok := s.u8()
		if !ok {
			return nil, false
		}
		n = int(v)
	default:
		v, ok := s.u16()
		if !ok {
			return nil, false
		}
		n = int(v)
	}
	if len(*s) < n {
		return nil, false
	}
	v := (*s)[:n]
	*s = (*s)[n:]
	return v, true
}

func (s *helloReader) skipVec(lenBytes int) bool {
	_, ok := s.vec(lenBytes)
	return ok
}

// judgeHello says why a hello may not open a tunnel to host, or "" when it
// may: its server name must be host, compared ASCII case-insensitively,
// and a tunnel to an address literal carries no name (RFC 6066 §3 forbids
// a literal there) unless it names that same address. A hello that offers
// an encrypted hello is refused, since the name the front end would route
// by is the one the proxy cannot read.
func judgeHello(h clientHello, host string) string {
	switch {
	case h.encrypted:
		return "it offers an encrypted ClientHello, whose server name the proxy cannot read"
	case h.serverName == "" && net.ParseIP(host) != nil:
		return ""
	case h.serverName == "":
		return "it names no server"
	case !asciiEqualFold(h.serverName, host):
		return "it names another host than the tunnel's"
	}
	return ""
}

// checkClientHello reads and judges the hello the client sends into a
// tunnel to host:port, within helloTimeout. It answers the bytes read when
// the tunnel may carry them, and logs the refusal otherwise.
func (p *Proxy) checkClientHello(client net.Conn, r io.Reader, host, port string) ([]byte, bool) {
	_ = client.SetReadDeadline(time.Now().Add(helloTimeout))
	raw, h, err := readClientHello(r)
	_ = client.SetReadDeadline(time.Time{})
	if err != nil {
		p.logf("deny sni for %s:%s (%v)", host, port, err)
		return nil, false
	}
	if why := judgeHello(h, host); why != "" {
		p.logf("deny sni %q for %s:%s (%s)", h.serverName, host, port, why)
		return nil, false
	}
	return raw, true
}

// asciiEqualFold compares two names case-insensitively in ASCII alone.
// strings.EqualFold applies Unicode folding, under which "ſ" (U+017F)
// matches "s", so "api.oſv.dev" would pass for "api.osv.dev".
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

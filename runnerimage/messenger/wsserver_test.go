package messenger

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // RFC 6455's handshake
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
)

// wsPeer is a test server's side of a WebSocket: it reads the client's
// masked frames and writes its own unmasked.
type wsPeer struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
}

// acceptWS upgrades r to a WebSocket, as a server does.
func acceptWS(t *testing.T, w http.ResponseWriter, r *http.Request) *wsPeer {
	t.Helper()
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Header.Get("Upgrade") != "websocket" || key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		t.Errorf("not a WebSocket handshake: %v", r.Header)
		http.Error(w, "no", http.StatusBadRequest)
		return nil
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // as above
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	_ = rw.Flush()
	return &wsPeer{c: conn, br: rw.Reader}
}

// read is the client's next text message, or false once it closed.
func (p *wsPeer) read() ([]byte, bool) {
	for {
		var h [2]byte
		if _, err := io.ReadFull(p.br, h[:]); err != nil {
			return nil, false
		}
		op := h[0] & 0x0f
		if h[1]&0x80 == 0 {
			return nil, false // a client's frame must be masked
		}
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			_, _ = io.ReadFull(p.br, b[:])
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			_, _ = io.ReadFull(p.br, b[:])
			n = binary.BigEndian.Uint64(b[:])
		}
		var mask [4]byte
		if _, err := io.ReadFull(p.br, mask[:]); err != nil {
			return nil, false
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(p.br, b); err != nil {
			return nil, false
		}
		for i := range b {
			b[i] ^= mask[i%4]
		}
		switch op {
		case wsText:
			return b, true
		case wsPing:
			p.frame(wsPong, b)
		case wsClose:
			return nil, false
		}
	}
}

// frame writes one unmasked frame.
func (p *wsPeer) frame(op byte, b []byte) {
	h := []byte{0x80 | op}
	switch n := len(b); {
	case n < 126:
		h = append(h, byte(n))
	case n <= 0xffff:
		h = append(h, 126)
		h = binary.BigEndian.AppendUint16(h, uint16(n))
	default:
		h = append(h, 127)
		h = binary.BigEndian.AppendUint64(h, uint64(n))
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, _ = p.c.Write(append(h, b...))
}

func (p *wsPeer) text(b []byte) { p.frame(wsText, b) }

// closeWith closes with code.
func (p *wsPeer) closeWith(code int) {
	p.frame(wsClose, binary.BigEndian.AppendUint16(nil, uint16(code)))
	_ = p.c.Close()
}

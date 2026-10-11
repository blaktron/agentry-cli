package source

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The pkt-line framing of git's wire protocol (gitprotocol-common): four
// hex digits giving the line's length including themselves, then the
// payload. Three lengths are special: 0000 flush, 0001 delimiter (protocol
// v2), 0002 response end (protocol v2).
const (
	pktFlush = iota
	pktDelim
	pktEnd
	pktData
)

// maxPktData is the largest payload a pkt-line carries.
const maxPktData = 65516

// pktReader reads pkt-lines from a server's answer.
type pktReader struct {
	r   io.Reader
	buf [maxPktData + 4]byte
}

// next reads one pkt-line: its kind and, for data, its payload, which is
// valid until the next call.
func (p *pktReader) next() (int, []byte, error) {
	if _, err := io.ReadFull(p.r, p.buf[:4]); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	n, err := strconv.ParseUint(string(p.buf[:4]), 16, 16)
	if err != nil {
		return 0, nil, fmt.Errorf("the host's answer is not git's pkt-line framing")
	}
	switch n {
	case 0:
		return pktFlush, nil, nil
	case 1:
		return pktDelim, nil, nil
	case 2:
		return pktEnd, nil, nil
	case 3:
		return 0, nil, fmt.Errorf("the host's answer holds an invalid pkt-line length")
	}
	if n > maxPktData+4 {
		return 0, nil, fmt.Errorf("the host's answer holds a pkt-line past git's limit")
	}
	data := p.buf[4:n]
	if _, err := io.ReadFull(p.r, data); err != nil {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return pktData, data, nil
}

// line reads one data line, its trailing newline removed, and fails on
// anything else; an ERR line is the host's own error.
func (p *pktReader) line() (string, int, error) {
	kind, data, err := p.next()
	if err != nil || kind != pktData {
		return "", kind, err
	}
	s := string(bytes.TrimSuffix(data, []byte("\n")))
	if msg, ok := cutPrefix(s, "ERR "); ok {
		return "", kind, hostError(msg)
	}
	return s, kind, nil
}

// pktWriter builds a request body.
type pktWriter struct{ bytes.Buffer }

func (w *pktWriter) line(s string) {
	_, _ = fmt.Fprintf(&w.Buffer, "%04x%s\n", len(s)+5, s)
}

func (w *pktWriter) flush() { w.WriteString("0000") }
func (w *pktWriter) delim() { w.WriteString("0001") }

// hostError is an error line the host sent (ERR, or band 3), passed on as
// printable text of bounded length: it is the host's words, shown to the
// person.
func hostError(msg string) error {
	return fmt.Errorf("the host said: %s", printable(msg, 300))
}

// printable keeps the printable ASCII of s, at most n bytes of it (the host
// keeps the same rule, pkg/runner/sources).
func printable(s string, n int) string {
	var b []byte
	for i := 0; i < len(s) && len(b) < n; i++ {
		if c := s[i]; c >= 0x20 && c < 0x7f {
			b = append(b, c)
		}
	}
	return string(b)
}

func cutPrefix(s, prefix string) (string, bool) { return strings.CutPrefix(s, prefix) }

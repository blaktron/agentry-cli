package toolsrv

// The content codings fetch decodes. A host may send a body encoded
// (Content-Encoding) when the caller set Accept-Encoding itself, and the
// transport then hands the bytes over as sent. The scrub of an answer, the
// check of a saved body and the watch behind saveTo all look for a granted
// secret's value in the bytes they are given, so a value inside a
// compressed body would pass them unseen. fetch therefore decodes the body
// first, so that every check sees the bytes the agent would read, and it
// refuses what it cannot decode rather than pass it through: more than one
// coding on a body, a coding other than gzip, deflate and br, and a body
// that decodes to more than a fixed multiple of the reply cap.

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/brotli"
)

// DecodedFetchFactor is how many times the reply cap a body may decode to.
// A compressor does well on text (ten to twenty times) and far better on
// a body built to be small encoded, so the limit sits well above real
// pages and far below what a few hundred encoded bytes can expand to.
const DecodedFetchFactor = 64

// decodedFetchCap is the most bytes an encoded body may decode to.
func (l Limits) decodedFetchCap() int64 { return l.fetchCap() * DecodedFetchFactor }

// errDecodedTooLarge marks a body that decodes past decodedFetchCap.
var errDecodedTooLarge = errors.New("the body decodes to more than the limit")

// codingRefused is a body fetch will not decode: the answer says why.
type codingRefused string

func (e codingRefused) Error() string { return string(e) }

// contentCodings is the codings a response names, in order, lower-cased
// and trimmed, without "identity", which is no coding at all.
func contentCodings(h http.Header) []string {
	var out []string
	for _, line := range h.Values("Content-Encoding") {
		for _, c := range strings.Split(line, ",") {
			c = strings.ToLower(strings.TrimSpace(c))
			if c != "" && c != "identity" {
				out = append(out, c)
			}
		}
	}
	return out
}

// decodedBody is the response body as the agent will read it, and the
// coding it was decoded from, or "" when it is the bytes as sent. The
// client's transport does no decoding of its own (a transport that decoded
// one layer of a stack would hide the header and pass the next layer on as
// plain bytes), so every coding a host names is seen here. A HEAD response
// has no body to decode.
func (s *server) decodedBody(method string, resp *http.Response) (io.Reader, string, error) {
	if method == http.MethodHead {
		return resp.Body, "", nil
	}
	var r io.Reader
	var coding string
	switch codings := contentCodings(resp.Header); {
	case len(codings) == 0:
		return resp.Body, "", nil
	case len(codings) > 1:
		return nil, "", codingRefused(fmt.Sprintf("the host sent the body with more than one content coding (%s); fetch decodes one and refuses a stack. Ask with the header Accept-Encoding: identity", clip(strings.Join(codings, ", "), maxFetchHeadField)))
	default:
		coding = codings[0]
		open := codingDecoder(coding)
		if open == nil {
			return nil, "", codingRefused(fmt.Sprintf("the host sent the body as %q, which fetch cannot decode (gzip, deflate and br it can). Ask with the header Accept-Encoding: identity", clip(coding, maxFetchHeadField)))
		}
		r = &lazyDecoder{src: resp.Body, open: open}
		if coding == "x-gzip" {
			coding = "gzip"
		}
	}
	return &boundedReader{r: r, left: s.spec.Limits.decodedFetchCap()}, coding, nil
}

// codingDecoder opens a decoder for a coding, or is nil for one fetch
// cannot decode.
func codingDecoder(coding string) func(io.Reader) (io.Reader, error) {
	switch coding {
	case "gzip", "x-gzip":
		return func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }
	case "deflate":
		return func(r io.Reader) (io.Reader, error) {
			// The coding is zlib-wrapped deflate; some hosts send the raw
			// stream, which the first two bytes tell apart.
			br := bufio.NewReader(r)
			head, err := br.Peek(2)
			if err != nil {
				return nil, fmt.Errorf("deflate: %w", io.ErrUnexpectedEOF)
			}
			if head[0]&0x0f == 8 && (uint(head[0])<<8|uint(head[1]))%31 == 0 {
				return zlib.NewReader(br)
			}
			return flate.NewReader(br), nil
		}
	case "br":
		return func(r io.Reader) (io.Reader, error) { return brotli.NewReader(r), nil }
	}
	return nil
}

// lazyDecoder opens its decoder on the first read, so that an empty body,
// which no decoder accepts, is an empty body.
type lazyDecoder struct {
	src  io.Reader
	open func(io.Reader) (io.Reader, error)
	r    io.Reader
	err  error
}

func (d *lazyDecoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.r == nil {
		br := bufio.NewReader(d.src)
		if _, err := br.Peek(1); err != nil {
			d.err = err
			return 0, err
		}
		r, err := d.open(br)
		if err != nil {
			d.err = err
			return 0, err
		}
		d.r = r
	}
	n, err := d.r.Read(p)
	if err != nil {
		d.err = err
	}
	return n, err
}

// boundedReader fails with errDecodedTooLarge on the first byte past its
// limit, never delivering that byte.
type boundedReader struct {
	r    io.Reader
	left int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.r.Read(p)
	if int64(n) > b.left {
		n = int(b.left)
		b.left = 0
		return n, errDecodedTooLarge
	}
	b.left -= int64(n)
	return n, err
}

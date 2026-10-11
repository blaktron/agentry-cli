package source

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1" //nolint:gosec // git's object names are SHA-1; this is identity, not security
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"
)

// A packfile as the host sent it (gitformat-pack), read from a file on
// disk: each object's place is indexed in one pass, every object's name is
// computed (deltas resolved against their bases), and an object is read
// back by name when the checkout needs it. Nothing the host claims is
// taken on trust: the pack's checksum, each object's inflated size and
// each delta's bounds are checked, and an object is known by the name its
// own bytes hash to.

// The object types.
const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

// maxDeltaObject bounds an object rebuilt from a delta, which is held in
// memory; an object stored whole is streamed and bounded by the checkout's
// byte cap alone.
const maxDeltaObject = 256 << 20

// maxChain bounds how many deltas one object may sit behind.
const maxChain = 512

var errPack = errors.New("the host sent a malformed pack")

// entry is one object's place in the pack.
type entry struct {
	off     int64 // where its header starts
	dataOff int64 // where its zlib stream starts
	typ     int   // as stored: a delta type for a delta
	size    int64 // as stored: a delta's own size for a delta
	baseOff int64 // an ofs-delta's base
	baseOID string
	// Filled once resolved: the object's own type, size and name.
	resolved bool
	ftyp     int
	fsize    int64
	oid      string
}

// pack is an indexed packfile.
type pack struct {
	f       *os.File
	entries []entry
	byOff   map[int64]int
	byOID   map[string]int
	cache   *baseCache
}

// checkTrailer verifies the pack's last 20 bytes are the SHA-1 of the rest.
func checkTrailer(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() < 32 {
		return errPack
	}
	h := sha1.New() //nolint:gosec // git's checksum
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, fi.Size()-20)); err != nil {
		return err
	}
	want := make([]byte, 20)
	if _, err := f.ReadAt(want, fi.Size()-20); err != nil {
		return err
	}
	if !bytes.Equal(h.Sum(nil), want) {
		return errors.New("the pack's checksum does not match its content")
	}
	return nil
}

// countReader counts what is consumed through it; flate reads it a byte at
// a time (it is an io.ByteReader), so the count is exactly where each zlib
// stream ends.
type countReader struct {
	br *bufio.Reader
	n  int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.br.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.br.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// indexPack reads every object header in the pack, inflating each stream
// once to find where it ends and to check its size, and names every object
// stored whole as it goes.
func indexPack(f *os.File, maxObjects int) (*pack, error) {
	if err := checkTrailer(f); err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	end := fi.Size() - 20
	cr := &countReader{br: bufio.NewReaderSize(io.NewSectionReader(f, 0, end), 1<<16)}
	var hdr [12]byte
	if _, err := io.ReadFull(cr, hdr[:]); err != nil || string(hdr[:4]) != "PACK" {
		return nil, errPack
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 && v != 3 {
		return nil, fmt.Errorf("the pack's version %d is not supported", v)
	}
	count := binary.BigEndian.Uint32(hdr[8:12])
	if int64(count) > int64(maxObjects) {
		return nil, fmt.Errorf("the pack holds %d objects, past the %d this fetch reads", count, maxObjects)
	}
	p := &pack{f: f, entries: make([]entry, 0, count), byOff: map[int64]int{}, byOID: map[string]int{}, cache: newBaseCache(64 << 20)}
	for i := uint32(0); i < count; i++ {
		e := entry{off: cr.n}
		c, err := cr.ReadByte()
		if err != nil {
			return nil, errPack
		}
		e.typ = int(c>>4) & 7
		e.size = int64(c & 15)
		shift := uint(4)
		for c&0x80 != 0 {
			if c, err = cr.ReadByte(); err != nil || shift > 56 {
				return nil, errPack
			}
			e.size |= int64(c&0x7f) << shift
			shift += 7
		}
		switch e.typ {
		case objCommit, objTree, objBlob, objTag:
		case objOfsDelta:
			c, err := cr.ReadByte()
			if err != nil {
				return nil, errPack
			}
			ofs := int64(c & 0x7f)
			for c&0x80 != 0 {
				if c, err = cr.ReadByte(); err != nil || ofs > 1<<48 {
					return nil, errPack
				}
				ofs = ((ofs + 1) << 7) | int64(c&0x7f)
			}
			e.baseOff = e.off - ofs
			if ofs <= 0 || e.baseOff < 12 {
				return nil, errPack
			}
		case objRefDelta:
			var b [20]byte
			if _, err := io.ReadFull(cr, b[:]); err != nil {
				return nil, errPack
			}
			e.baseOID = hex.EncodeToString(b[:])
		default:
			return nil, errPack
		}
		e.dataOff = cr.n
		var h hash.Hash
		if e.typ != objOfsDelta && e.typ != objRefDelta {
			h = sha1.New() //nolint:gosec // git's object name
			_, _ = fmt.Fprintf(h, "%s %d\x00", typeName(e.typ), e.size)
		}
		if err := inflateCount(cr, e.size, h); err != nil {
			return nil, err
		}
		if h != nil {
			e.resolved, e.ftyp, e.fsize, e.oid = true, e.typ, e.size, hex.EncodeToString(h.Sum(nil))
			p.byOID[e.oid] = len(p.entries)
		}
		p.byOff[e.off] = len(p.entries)
		p.entries = append(p.entries, e)
	}
	if cr.n != end {
		return nil, errPack
	}
	return p, p.resolveDeltas()
}

// inflateCount inflates one zlib stream, which must give exactly size
// bytes, into h when there is one.
func inflateCount(r *countReader, size int64, h hash.Hash) error {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return errPack
	}
	w := io.Discard
	if h != nil {
		w = h
	}
	n, err := io.Copy(w, io.LimitReader(zr, size+1))
	if err != nil || n != size {
		return errPack
	}
	// Read to the stream's end, so its checksum is checked and consumed.
	if _, err := zr.Read(make([]byte, 1)); err != io.EOF {
		return errPack
	}
	return zr.Close()
}

// resolveDeltas names every delta: those whose base is known are rebuilt
// and hashed, in rounds until none is left. A delta whose base the pack
// does not hold (a thin pack, which this client never asks for) fails the
// fetch.
func (p *pack) resolveDeltas() error {
	for {
		progress, left := false, 0
		for i := range p.entries {
			e := &p.entries[i]
			if e.resolved {
				continue
			}
			base, ok := p.baseOf(e)
			if !ok || !p.entries[base].resolved {
				left++
				continue
			}
			typ, data, err := p.load(i, 0)
			if err != nil {
				return err
			}
			h := sha1.New() //nolint:gosec // git's object name
			_, _ = fmt.Fprintf(h, "%s %d\x00", typeName(typ), len(data))
			h.Write(data)
			e.resolved, e.ftyp, e.fsize, e.oid = true, typ, int64(len(data)), hex.EncodeToString(h.Sum(nil))
			p.byOID[e.oid] = i
			progress = true
		}
		if left == 0 {
			return nil
		}
		if !progress {
			return errors.New("the pack holds a delta against an object it does not hold")
		}
	}
}

// baseOf is a delta's base entry.
func (p *pack) baseOf(e *entry) (int, bool) {
	if e.typ == objOfsDelta {
		i, ok := p.byOff[e.baseOff]
		return i, ok
	}
	i, ok := p.byOID[e.baseOID]
	return i, ok
}

// load answers an object's type and bytes, rebuilding a delta from its
// base. Only a delta's object and its bases are held in memory, bounded by
// maxDeltaObject; depth bounds the chain.
func (p *pack) load(i, depth int) (int, []byte, error) {
	if depth > maxChain {
		return 0, nil, errors.New("the pack's delta chain is too deep")
	}
	e := &p.entries[i]
	if e.typ != objOfsDelta && e.typ != objRefDelta {
		if e.size > maxDeltaObject {
			return 0, nil, fmt.Errorf("an object of %d bytes is past the %d this fetch holds in memory", e.size, maxDeltaObject)
		}
		data, err := p.inflate(e)
		return e.typ, data, err
	}
	if e.size > maxDeltaObject {
		return 0, nil, fmt.Errorf("a delta of %d bytes is past the %d this fetch holds in memory", e.size, maxDeltaObject)
	}
	if data, typ, ok := p.cache.get(e.off); ok {
		return typ, data, nil
	}
	bi, ok := p.baseOf(e)
	if !ok {
		return 0, nil, errors.New("the pack holds a delta against an object it does not hold")
	}
	btyp, base, err := p.load(bi, depth+1)
	if err != nil {
		return 0, nil, err
	}
	delta, err := p.inflate(e)
	if err != nil {
		return 0, nil, err
	}
	out, err := applyDelta(base, delta)
	if err != nil {
		return 0, nil, err
	}
	p.cache.put(e.off, out, btyp)
	return btyp, out, nil
}

// inflate reads one stored stream whole.
func (p *pack) inflate(e *entry) ([]byte, error) {
	zr, err := zlib.NewReader(io.NewSectionReader(p.f, e.dataOff, 1<<62))
	if err != nil {
		return nil, errPack
	}
	defer func() { _ = zr.Close() }()
	buf := make([]byte, e.size)
	if _, err := io.ReadFull(zr, buf); err != nil {
		return nil, errPack
	}
	return buf, nil
}

// object answers a named object's type and bytes.
func (p *pack) object(oid string) (int, []byte, error) {
	i, ok := p.byOID[oid]
	if !ok {
		return 0, nil, fmt.Errorf("the pack does not hold %s", oid)
	}
	return p.load(i, 0)
}

// writeBlob writes a named blob to w: streamed from the pack when it is
// stored whole, rebuilt when it is a delta.
func (p *pack) writeBlob(oid string, w io.Writer) error {
	i, ok := p.byOID[oid]
	if !ok {
		return fmt.Errorf("the pack does not hold %s", oid)
	}
	e := &p.entries[i]
	if e.ftyp != objBlob {
		return fmt.Errorf("%s is not a file's content", oid)
	}
	if e.typ == objBlob {
		zr, err := zlib.NewReader(io.NewSectionReader(p.f, e.dataOff, 1<<62))
		if err != nil {
			return errPack
		}
		defer func() { _ = zr.Close() }()
		n, err := io.Copy(w, io.LimitReader(zr, e.size))
		if err != nil {
			return err
		}
		if n != e.size {
			return errPack
		}
		return nil
	}
	_, data, err := p.load(i, 0)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// applyDelta rebuilds an object from its base and a delta
// (gitformat-pack, "Deltified representation"), every copy held to the
// base's bounds and the result to the size the delta declares.
func applyDelta(base, delta []byte) ([]byte, error) {
	src, delta, ok := deltaSize(delta)
	if !ok || src != int64(len(base)) {
		return nil, errPack
	}
	dst, delta, ok := deltaSize(delta)
	if !ok || dst > maxDeltaObject {
		return nil, errPack
	}
	out := make([]byte, 0, dst)
	for len(delta) > 0 {
		op := delta[0]
		delta = delta[1:]
		switch {
		case op&0x80 != 0:
			var off, n int64
			for b := uint(0); b < 4; b++ {
				if op&(1<<b) != 0 {
					if len(delta) == 0 {
						return nil, errPack
					}
					off |= int64(delta[0]) << (8 * b)
					delta = delta[1:]
				}
			}
			for b := uint(0); b < 3; b++ {
				if op&(0x10<<b) != 0 {
					if len(delta) == 0 {
						return nil, errPack
					}
					n |= int64(delta[0]) << (8 * b)
					delta = delta[1:]
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if off+n > int64(len(base)) || int64(len(out))+n > dst {
				return nil, errPack
			}
			out = append(out, base[off:off+n]...)
		case op != 0:
			n := int(op)
			if n > len(delta) || int64(len(out)+n) > dst {
				return nil, errPack
			}
			out = append(out, delta[:n]...)
			delta = delta[n:]
		default:
			return nil, errPack
		}
	}
	if int64(len(out)) != dst {
		return nil, errPack
	}
	return out, nil
}

// deltaSize reads one of a delta's two leading sizes.
func deltaSize(b []byte) (int64, []byte, bool) {
	var v int64
	shift := uint(0)
	for i, c := range b {
		if shift > 56 {
			return 0, nil, false
		}
		v |= int64(c&0x7f) << shift
		shift += 7
		if c&0x80 == 0 {
			return v, b[i+1:], true
		}
	}
	return 0, nil, false
}

func typeName(t int) string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return "type" + strconv.Itoa(t)
}

// baseCache keeps rebuilt objects by pack offset, within a byte budget,
// evicting the oldest first: a chain of deltas against one base rebuilds
// it once.
type baseCache struct {
	budget, used int64
	order        []int64
	items        map[int64]cached
}

type cached struct {
	data []byte
	typ  int
}

func newBaseCache(budget int64) *baseCache {
	return &baseCache{budget: budget, items: map[int64]cached{}}
}

func (c *baseCache) get(off int64) ([]byte, int, bool) {
	it, ok := c.items[off]
	return it.data, it.typ, ok
}

func (c *baseCache) put(off int64, data []byte, typ int) {
	if int64(len(data)) > c.budget {
		return
	}
	if _, ok := c.items[off]; ok {
		return
	}
	for c.used+int64(len(data)) > c.budget && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		c.used -= int64(len(c.items[old].data))
		delete(c.items, old)
	}
	c.items[off] = cached{data: data, typ: typ}
	c.order = append(c.order, off)
	c.used += int64(len(data))
}

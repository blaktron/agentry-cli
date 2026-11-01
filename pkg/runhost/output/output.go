// Package output is a remote run's output at rest on the runhost: the run's
// output folder packed into one tar of the members a device takes back, and
// that tar sealed to every paired device's sealing key, so the server keeps
// no plaintext output once the run has ended.
//
// The sealed layout, all integers big-endian:
//
//	magic    "AGYRHO1\n"
//	uint32   header length, then the header (JSON, Header)
//	frames:  uint32 length, then that many bytes of ciphertext
//
// The tar is encrypted with a random content key, AES-256-GCM, in frames of
// at most ChunkSize bytes of plaintext. Each frame's nonce is its index, and
// its additional data is the header's SHA-256, the index and whether it is
// the last, so a frame cannot be moved, dropped or repeated, a stream cut at
// a frame boundary does not open, and a header changed in any byte (a
// recipient added, the job renamed) opens no frame. The content key is
// sealed to each device with HPKE (RFC 9180: DHKEM(X25519, HKDF-SHA256),
// HKDF-SHA256, AES-256-GCM), whose info names the job and the device, so a
// key sealed for one job or device does not open as another's.
package output

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Magic opens every sealed output.
const Magic = "AGYRHO1\n"

// ChunkSize is the most plaintext one frame carries.
const ChunkSize = 64 << 10

// MaxHeader bounds the header a reader accepts.
const MaxHeader = 64 << 10

const (
	suiteKEM  = "DHKEM(X25519, HKDF-SHA256)"
	suiteKDF  = "HKDF-SHA256"
	suiteAEAD = "AES-256-GCM"
)

// Header is what can be read of a sealed output without a key: the job it
// was sealed for and the devices it was sealed to.
type Header struct {
	Version    int         `json:"v"`
	KEM        string      `json:"kem"`
	KDF        string      `json:"kdf"`
	AEAD       string      `json:"aead"`
	Job        string      `json:"job"`
	Chunk      int         `json:"chunk"`
	Recipients []Recipient `json:"recipients"`
}

// Recipient is one device's sealed copy of the content key.
type Recipient struct {
	Device string `json:"device"`
	Enc    []byte `json:"enc"`
	Key    []byte `json:"key"`
}

// To is a device an output is sealed to.
type To struct {
	Device string
	Key    *ecdh.PublicKey
}

var (
	// ErrFormat is a stream that is not a sealed output of this version.
	ErrFormat = errors.New("not a sealed runhost output")
	// ErrOpen is every failure to open one: not sealed to this device, for
	// another job, a frame altered, moved or missing. Which check failed is
	// not the caller's to know.
	ErrOpen = errors.New("the sealed output does not open with this device's key")
)

func keyInfo(job, device string) []byte {
	return []byte("agentry/runhost-output/v1\x00" + job + "\x00" + device)
}

func frameAAD(headerSum [32]byte, index uint64, last bool) []byte {
	aad := make([]byte, 0, 32+8+1)
	aad = append(aad, headerSum[:]...)
	aad = binary.BigEndian.AppendUint64(aad, index)
	if last {
		return append(aad, 1)
	}
	return append(aad, 0)
}

func frameNonce(index uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], index)
	return n
}

// Seal writes plaintext, read to its end, to w sealed to every device in to
// for job.
func Seal(w io.Writer, plaintext io.Reader, to []To, job string) error {
	if len(to) == 0 {
		return errors.New("an output must be sealed to at least one device")
	}
	var ck [32]byte
	if _, err := rand.Read(ck[:]); err != nil {
		return err
	}
	h := Header{Version: 1, KEM: suiteKEM, KDF: suiteKDF, AEAD: suiteAEAD, Job: job, Chunk: ChunkSize}
	for _, t := range to {
		pk, err := hpke.NewDHKEMPublicKey(t.Key)
		if err != nil {
			return err
		}
		enc, sender, err := hpke.NewSender(pk, hpke.HKDFSHA256(), hpke.AES256GCM(), keyInfo(job, t.Device))
		if err != nil {
			return err
		}
		wrapped, err := sender.Seal(nil, ck[:])
		if err != nil {
			return err
		}
		h.Recipients = append(h.Recipients, Recipient{Device: t.Device, Enc: enc, Key: wrapped})
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if len(hb) > MaxHeader {
		return errors.New("too many devices for one sealed output")
	}
	aead, err := newAEAD(ck[:])
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, ChunkSize+64)
	if _, err := bw.WriteString(Magic); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, uint32(len(hb))); err != nil {
		return err
	}
	if _, err := bw.Write(hb); err != nil {
		return err
	}
	sum := sha256.Sum256(hb)
	// Read one chunk ahead, so each frame knows whether it is the last.
	cur, next := make([]byte, ChunkSize), make([]byte, ChunkSize)
	n, err := io.ReadFull(plaintext, cur)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	for index := uint64(0); ; index++ {
		m := 0
		last := n < ChunkSize
		if !last {
			m, err = io.ReadFull(plaintext, next)
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return err
			}
			last = m == 0
		}
		ct := aead.Seal(nil, frameNonce(index), cur[:n], frameAAD(sum, index, last))
		if err := binary.Write(bw, binary.BigEndian, uint32(len(ct))); err != nil {
			return err
		}
		if _, err := bw.Write(ct); err != nil {
			return err
		}
		if last {
			break
		}
		cur, next, n = next, cur, m
	}
	return bw.Flush()
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// ReadHeader reads the magic and the header from r and leaves r at the
// first frame.
func ReadHeader(r io.Reader) (Header, []byte, error) {
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != Magic {
		return Header{}, nil, ErrFormat
	}
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil || n == 0 || n > MaxHeader {
		return Header{}, nil, ErrFormat
	}
	hb := make([]byte, n)
	if _, err := io.ReadFull(r, hb); err != nil {
		return Header{}, nil, ErrFormat
	}
	var h Header
	if err := json.Unmarshal(hb, &h); err != nil || h.Version != 1 || h.KEM != suiteKEM || h.KDF != suiteKDF || h.AEAD != suiteAEAD || h.Chunk != ChunkSize || len(h.Recipients) == 0 {
		return Header{}, nil, ErrFormat
	}
	return h, hb, nil
}

// Open opens a sealed output r with device's sealing key, for job, and
// writes its plaintext to w. It fails before writing anything when the
// output was not sealed to this device or for this job; an error after
// that means w holds part of the plaintext, which the caller must discard.
func Open(w io.Writer, r io.Reader, device string, key *ecdh.PrivateKey, job string) error {
	h, hb, err := ReadHeader(r)
	if err != nil {
		return err
	}
	if h.Job != job {
		return ErrOpen
	}
	var ck []byte
	for _, rc := range h.Recipients {
		if rc.Device != device {
			continue
		}
		sk, err := hpke.NewDHKEMPrivateKey(key)
		if err != nil {
			return err
		}
		rcpt, err := hpke.NewRecipient(rc.Enc, sk, hpke.HKDFSHA256(), hpke.AES256GCM(), keyInfo(job, device))
		if err != nil {
			return ErrOpen
		}
		if ck, err = rcpt.Open(nil, rc.Key); err != nil || len(ck) != 32 {
			return ErrOpen
		}
		break
	}
	if ck == nil {
		return ErrOpen
	}
	aead, err := newAEAD(ck)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(hb)
	br := bufio.NewReaderSize(r, ChunkSize+64)
	for index := uint64(0); ; index++ {
		var n uint32
		if err := binary.Read(br, binary.BigEndian, &n); err != nil || n < 16 || n > ChunkSize+16 {
			return ErrOpen
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(br, ct); err != nil {
			return ErrOpen
		}
		// A frame says whether it is the last only through its additional
		// data, so try both: exactly one opens.
		var pt []byte
		opened, last := false, false
		for _, l := range []bool{false, true} {
			if p, err := aead.Open(nil, frameNonce(index), ct, frameAAD(sum, index, l)); err == nil {
				pt, opened, last = p, true, l
				break
			}
		}
		if !opened {
			return ErrOpen
		}
		if _, err := w.Write(pt); err != nil {
			return err
		}
		if last {
			if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
				return ErrOpen // bytes after the last frame
			}
			return nil
		}
	}
}

// Devices is the To list for the paired devices' sealing keys, base64 X25519
// public keys by device id.
func Devices(keys map[string][]byte) ([]To, error) {
	var out []To
	for id, raw := range keys {
		pk, err := ecdh.X25519().NewPublicKey(raw)
		if err != nil {
			return nil, fmt.Errorf("device %s's sealing key is not an X25519 key: %w", id, err)
		}
		out = append(out, To{Device: id, Key: pk})
	}
	return out, nil
}

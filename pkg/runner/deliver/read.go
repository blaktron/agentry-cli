package deliver

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// ReadError is why a deliverable could not be read: one of receipt's closed
// words (DeliveryErrChanged, DeliveryErrMissing, DeliveryErrOutside,
// DeliveryErrLink, DeliveryErrUnreadable, DeliveryErrType, DeliveryErrTooLarge).
type ReadError struct{ Reason string }

func (e *ReadError) Error() string { return "deliverable not read: " + e.Reason }

// Locate checks a deliverable of the run folder runDir the way a delivery
// does, and gives the absolute path of the file it names. The file must be a
// regular file under reports/, reached through no link, of a type a file
// manager does not act on, and its bytes must be the size and SHA-256 the
// envelope lists. Nothing is written. It is for a client that hands the path
// to a person or a program, which is why it holds the file to the digest now.
func Locate(runDir string, f receipt.Deliverable) (string, error) {
	abs, _, err := verify(runDir, f, 0)
	return abs, err
}

// Read is Locate and the file's bytes, which it reads once and holds to the
// digest; a file larger than max is refused (DeliveryErrTooLarge) before any
// byte is read.
func Read(runDir string, f receipt.Deliverable, max int64) (string, []byte, error) {
	if max <= 0 {
		return "", nil, &ReadError{Reason: receipt.DeliveryErrTooLarge}
	}
	return verify(runDir, f, max)
}

func verify(runDir string, f receipt.Deliverable, keepUpTo int64) (string, []byte, error) {
	rel := path.Clean(f.Path)
	if rel == "reports" || !strings.HasPrefix(rel, "reports/") || path.IsAbs(f.Path) || strings.Contains(f.Path, `\`) {
		return "", nil, &ReadError{Reason: receipt.DeliveryErrOutside}
	}
	if dangerousTypes[extType(rel)] {
		return "", nil, &ReadError{Reason: receipt.DeliveryErrType}
	}
	if keepUpTo > 0 && f.Bytes > keepUpTo {
		return "", nil, &ReadError{Reason: receipt.DeliveryErrTooLarge}
	}
	abs, err := filepath.Abs(runDir)
	if err != nil {
		return "", nil, &ReadError{Reason: receipt.DeliveryErrUnreadable}
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return "", nil, &ReadError{Reason: receipt.DeliveryErrUnreadable}
	}
	defer func() { _ = root.Close() }()
	in, reason := openNoLinks(root, rel)
	if reason != "" {
		return "", nil, &ReadError{Reason: reason}
	}
	defer func() { _ = in.Close() }()
	h := sha256.New()
	var kept []byte
	var n int64
	buf := make([]byte, 32<<10)
	lr := io.LimitReader(in, f.Bytes+1)
	for {
		m, rerr := lr.Read(buf)
		if m > 0 {
			n += int64(m)
			_, _ = h.Write(buf[:m])
			if keepUpTo > 0 {
				kept = append(kept, buf[:m]...)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", nil, &ReadError{Reason: receipt.DeliveryErrUnreadable}
		}
	}
	if n != f.Bytes || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return "", nil, &ReadError{Reason: receipt.DeliveryErrChanged}
	}
	return filepath.Join(abs, filepath.FromSlash(rel)), kept, nil
}

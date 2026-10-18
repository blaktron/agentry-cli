package deliver

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// Staged is what became of one deliverable on its way to a staging folder:
// the name of the verified copy there, or the closed reason there is none.
type Staged struct {
	Name   string
	Reason string
}

// Stage copies deliverables from the run folder into dir, as many as a body of
// budget bytes can carry (each file counts as the base64 of its bytes, which is
// how a webhook sends it), in order, and no more: a file that does not fit in
// what is left is not staged, whatever follows it. Each copy is made (which must exist), under names of the runner's own
// (f0000, f0001, …), and answers one Staged per file in order. A file is staged
// only as a delivery to a folder would copy it: read through no link, from
// beneath reports/, and shown to be the bytes of the envelope's size and
// SHA-256 as they are copied. A destination that sends a file reads the staged
// copy, never the run folder, so a file the agent changes after the check is
// never the one sent, and the step that holds the destination's credential
// sees no path of the run folder at all.
func Stage(runDir string, files []receipt.Deliverable, dir string, budget int64) []Staged {
	out := make([]Staged, len(files))
	src, err := os.OpenRoot(runDir)
	if err != nil {
		for i := range out {
			out[i].Reason = receipt.DeliveryErrUnreadable
		}
		return out
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenRoot(dir)
	if err != nil {
		for i := range out {
			out[i].Reason = receipt.DeliveryErrWrite
		}
		return out
	}
	defer func() { _ = dst.Close() }()
	for i, f := range files {
		rel := path.Clean(f.Path)
		switch {
		case rel == "reports" || !strings.HasPrefix(rel, "reports/") || strings.Contains(rel, "\\"):
			out[i].Reason = receipt.DeliveryErrOutside
			continue
		case f.Bytes < 0 || encodedSize(f.Bytes) > budget:
			out[i].Reason = receipt.DeliveryErrTooLarge
			continue
		}
		in, reason := openNoLinks(src, rel)
		if reason != "" {
			out[i].Reason = reason
			continue
		}
		name := fmt.Sprintf("f%04d", i)
		w, err := dst.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			_ = in.Close()
			out[i].Reason = receipt.DeliveryErrWrite
			continue
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(in, f.Bytes+1))
		_ = in.Close()
		cerr := w.Close()
		switch {
		case err != nil || cerr != nil:
			_ = dst.Remove(name)
			out[i].Reason = receipt.DeliveryErrWrite
		case n != f.Bytes || hex.EncodeToString(h.Sum(nil)) != f.SHA256:
			_ = dst.Remove(name)
			out[i].Reason = receipt.DeliveryErrChanged
		default:
			out[i].Name = name
			budget -= encodedSize(f.Bytes)
		}
	}
	return out
}

// encodedSize is what n bytes of a file take in a body: their base64.
func encodedSize(n int64) int64 {
	if n > 1<<40 {
		return n
	}
	return int64(base64.StdEncoding.EncodedLen(int(n)))
}

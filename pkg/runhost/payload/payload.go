// Package payload carries the files a run on a remote runhost takes from the
// device: its inputs, the files that answer its intake questions, the
// knowledge packs it attaches, its Writing Voice and its report design. The
// device packs them into one tar, which follows the job's document (and its
// release, when the runhost asks for it) and is laid down in the job's
// private directory on the server.
//
// A member's name is one of the kinds below, then a name of its own:
// inputs/<name>…, intake/<id>/<name>…, knowledge/<pack>/…, voice/VOICE.md and
// design/report-design.json. Only regular files and directories travel.
package payload

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// MaxPayload bounds the files one run takes: the same as the most output a
// run gives back.
const MaxPayload = 4 << 30

// The kinds of member, each the first segment of a member's name.
const (
	KindInputs    = "inputs"
	KindIntake    = "intake"
	KindKnowledge = "knowledge"
	KindVoice     = "voice"
	KindDesign    = "design"
)

var kinds = []string{KindInputs, KindIntake, KindKnowledge, KindVoice, KindDesign}

// Entry is one file or directory of the device's, packed under Name.
type Entry struct {
	// Name is the member's slash-separated name: a kind, then its own.
	Name string
	// Src is the file or directory on the device. A link to a regular file
	// is read through, as a local run copies one; a link inside a directory
	// to a directory is not followed.
	Src string
}

// CleanName is name when it is a clean relative member name under one of the
// kinds, else "".
func CleanName(name string) string {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) || len(name) > 4096 {
		return ""
	}
	c := path.Clean(name)
	if c != name || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return ""
	}
	kind, rest, ok := strings.Cut(c, "/")
	if !ok || rest == "" || !slices.Contains(kinds, kind) {
		return ""
	}
	return c
}

// Packed is a payload packed into a file of its own, ready to send.
type Packed struct {
	// Path is the packed tar, owner-only, which Remove deletes.
	Path   string
	Size   int64
	Digest string
	dir    string
}

// Remove deletes the packed tar.
func (p *Packed) Remove() {
	if p != nil && p.dir != "" {
		_ = os.RemoveAll(p.dir)
	}
}

// Open opens the packed tar for sending.
func (p *Packed) Open() (*os.File, error) { return os.Open(p.Path) }

// Pack packs entries into a tar in a private temporary directory and answers
// its size and digest. Packing first, rather than while sending, means a file
// that changes meanwhile cannot make the stream disagree with the size the job
// names.
func Pack(entries []Entry) (*Packed, error) {
	seen := map[string]bool{}
	for _, e := range entries {
		if CleanName(e.Name) == "" {
			return nil, fmt.Errorf("%q is not a payload member's name", e.Name)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("two of the run's files are both %s", e.Name)
		}
		seen[e.Name] = true
	}
	dir, err := os.MkdirTemp("", "agentry-runhost-payload-*")
	if err != nil {
		return nil, err
	}
	p := &Packed{dir: dir, Path: filepath.Join(dir, "payload.tar")}
	f, err := os.OpenFile(p.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		p.Remove()
		return nil, err
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	tw := tar.NewWriter(cw)
	var total int64
	for _, e := range entries {
		if err := packEntry(tw, e, &total); err != nil {
			_ = f.Close()
			p.Remove()
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		p.Remove()
		return nil, err
	}
	if err := f.Close(); err != nil {
		p.Remove()
		return nil, err
	}
	p.Size, p.Digest = cw.n, "sha256:"+hex.EncodeToString(h.Sum(nil))
	return p, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

func packEntry(tw *tar.Writer, e Entry, total *int64) error {
	fi, err := os.Stat(e.Src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return packFile(tw, e.Name, e.Src, fi, total)
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: e.Name + "/", Mode: 0o755, ModTime: fi.ModTime()}); err != nil {
		return err
	}
	return filepath.WalkDir(e.Src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(e.Src, p)
		if err != nil || rel == "." {
			return err
		}
		name := e.Name + "/" + filepath.ToSlash(rel)
		if CleanName(name) == "" {
			return fmt.Errorf("%s holds %q, which a run's files cannot carry", e.Src, rel)
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: info.ModTime()})
		}
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			// A link to a directory, a socket or a device: a local run's copy
			// skips what it cannot read as a file, and so does this.
			return nil
		}
		return packFile(tw, name, p, info, total)
	})
}

func packFile(tw *tar.Writer, name, src string, fi os.FileInfo, total *int64) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file or a directory", src)
	}
	if *total += fi.Size(); *total > MaxPayload {
		return fmt.Errorf("the run's files are over %d bytes", int64(MaxPayload))
	}
	f, err := os.Open(src) //nolint:gosec // a file the person gave the run
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(fi.Mode().Perm() | 0o600), Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, f, fi.Size()); err != nil {
		return fmt.Errorf("%s changed while it was packed: %w", src, err)
	}
	return nil
}

// Unpack reads exactly size bytes of r, a tar Pack wrote whose digest is
// digest, into dir, which it creates owner-only: regular files and
// directories only, each under a clean member name, so no entry lands
// outside dir. The digest is checked once the stream ends; a payload that
// does not match leaves dir to its caller to remove.
func Unpack(r io.Reader, size int64, digest, dir string) error {
	if size <= 0 || size > MaxPayload+MaxPayload/8 {
		return fmt.Errorf("a payload of %d bytes is not one a run takes", size)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	h := sha256.New()
	lr := &io.LimitedReader{R: r, N: size}
	tr := tar.NewReader(io.TeeReader(lr, h))
	var total int64
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("the run's files did not arrive whole: %w", err)
		}
		name := CleanName(strings.TrimSuffix(hd.Name, "/"))
		if name == "" {
			return fmt.Errorf("the run's files hold %q, which is not a member a run takes", hd.Name)
		}
		switch hd.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(filepath.FromSlash(name), 0o700); err != nil {
				return err
			}
			continue
		case tar.TypeReg:
		default:
			return fmt.Errorf("the run's files hold %q, which is neither a file nor a directory", hd.Name)
		}
		if total += hd.Size; hd.Size < 0 || total > MaxPayload {
			return fmt.Errorf("the run's files are over %d bytes", int64(MaxPayload))
		}
		if d := path.Dir(name); d != "." {
			if err := root.MkdirAll(filepath.FromSlash(d), 0o700); err != nil {
				return err
			}
		}
		f, err := root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, cerr := io.CopyN(f, tr, hd.Size)
		if err := f.Close(); cerr == nil {
			cerr = err
		}
		if cerr != nil {
			return cerr
		}
	}
	// The tar's end-of-archive blocks may stop short of the bytes the job
	// named; whatever is left is read, so the digest covers the whole stream.
	if _, err := io.Copy(h, lr); err != nil {
		return err
	}
	if lr.N != 0 {
		return errors.New("the run's files were cut short")
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("the run's files are %s, not the %s the job names", got, digest)
	}
	return nil
}

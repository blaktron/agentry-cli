package knowledge

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxFiles caps the files in a pack's tree: the registry refuses a tree
// with more (DEV-149), so no client keeps one either.
const MaxFiles = 2000

// ExtractTree reads a canonical tree tar — what WriteTree writes, and what
// a sealed pack opens to — from r into dst, and checks it is the tree want
// names. dst must not exist, or be an empty directory; it is made 0700,
// since what lands in it is a pack's words in the clear.
//
// Every entry must be a regular file with a clean slash path under wiki/
// or sources/, in byte order, each once; the whole tree at most MaxLocalBytes
// and MaxFiles. The stream's SHA-256 must be want, and the files written
// must re-encode to the same digest, so the bytes were the canonical tar
// of their files and nothing else ("one pack, one digest"). Every write
// goes through an os.Root at dst. A refused tree leaves nothing: a
// directory ExtractTree made is removed, and an empty one it was given is
// emptied again.
func ExtractTree(r io.Reader, dst, want string) (d Digest, err error) {
	made, err := prepareDir(dst)
	if err != nil {
		return Digest{}, err
	}
	defer func() {
		if err != nil {
			cleanup(dst, made)
		}
	}()
	root, err := os.OpenRoot(dst)
	if err != nil {
		return Digest{}, err
	}
	defer func() { _ = root.Close() }()

	h := sha256.New()
	stream := io.TeeReader(r, h)
	tr := tar.NewReader(stream)
	prev := ""
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Digest{}, fmt.Errorf("knowledge tree: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return Digest{}, fmt.Errorf("knowledge tree: %q is not a regular file; a knowledge pack holds only files", hdr.Name)
		}
		if err := treePath(hdr.Name); err != nil {
			return Digest{}, err
		}
		if hdr.Name <= prev {
			return Digest{}, fmt.Errorf("knowledge tree: %s is out of byte order or repeated", hdr.Name)
		}
		prev = hdr.Name
		if d.Files++; d.Files > MaxFiles {
			return Digest{}, fmt.Errorf("knowledge tree: more than %d files", MaxFiles)
		}
		// A download is bounded to Agentry's cap before it gets here
		// (remote); a pack opened from the store may be a local one.
		if hdr.Size < 0 || d.Bytes+hdr.Size > localCap {
			return Digest{}, fmt.Errorf("knowledge tree: past %s: %w", FormatCap(localCap), ErrTooLarge)
		}
		if err := writeTreeFile(root, hdr.Name, tr, hdr.Size); err != nil {
			return Digest{}, fmt.Errorf("knowledge tree: %s: %w", hdr.Name, err)
		}
		d.Bytes += hdr.Size
	}
	// The hash must cover every byte offered, so anything after the tar's
	// end changes the digest rather than passing unread.
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return Digest{}, fmt.Errorf("knowledge tree: %w", err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != want {
		return Digest{}, fmt.Errorf("knowledge tree: the tree's digest is %s, not %s", got, want)
	}
	again, err := TreeDigest(dst)
	if err != nil {
		return Digest{}, fmt.Errorf("knowledge tree: %w", err)
	}
	if again.SHA256 != want {
		return Digest{}, errors.New("knowledge tree: the tree is not the canonical tar of its files")
	}
	d.SHA256 = want
	return d, nil
}

// treePath holds an entry's name to a clean slash path under wiki/ or
// sources/: never absolute, no empty, "." or ".." segment, no backslash,
// colon or control character.
func treePath(name string) error {
	bad := name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:")
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			bad = true
		}
	}
	for seg := range strings.SplitSeq(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("knowledge tree: %q is not a clean relative path", name)
	}
	if !strings.HasPrefix(name, WikiDir+"/") && !strings.HasPrefix(name, SourcesDir+"/") {
		return fmt.Errorf("knowledge tree: %s is not under wiki/ or sources/", name)
	}
	return nil
}

// writeTreeFile writes one file of size bytes from r at rel under root.
func writeTreeFile(root *os.Root, rel string, r io.Reader, size int64) error {
	if i := strings.LastIndex(rel, "/"); i > 0 {
		if err := root.MkdirAll(rel[:i], 0o700); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.CopyN(f, r, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// prepareDir makes dst (0700), or accepts it as an empty directory; made
// says which.
func prepareDir(dst string) (made bool, err error) {
	if err := os.Mkdir(dst, 0o700); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrExist) {
		return false, err
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		return false, err
	}
	if len(entries) > 0 {
		return false, fmt.Errorf("knowledge tree: %s is not empty", dst)
	}
	return false, nil
}

// cleanup undoes a refused extraction: the directory goes if ExtractTree
// made it, else what was written into it.
func cleanup(dst string, made bool) {
	if made {
		_ = os.RemoveAll(dst)
		return
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dst, e.Name()))
	}
}

// Unpack makes the pack directory dst from a canonical tree tar whose
// digest is want: the tree extracted and checked (ExtractTree), a
// manifest written from m with the tree's own schema, counts and digest
// in place of whatever m said, and the pack loaded. It is how a pack kept
// somewhere as a tree — a registry's copy downloaded, a sandbox host's
// staged tree for a hosted build (agentry-cli#364) — becomes a directory
// the Creator and the runner read. A refused tree or pack leaves nothing
// in dst.
func Unpack(r io.Reader, want, dst string, m Manifest) (p *Pack, err error) {
	d, err := ExtractTree(r, dst, want)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			entries, _ := os.ReadDir(dst)
			for _, e := range entries {
				_ = os.RemoveAll(filepath.Join(dst, e.Name()))
			}
		}
	}()
	pages, err := Pages(dst)
	if err != nil {
		return nil, err
	}
	m.Schema, m.Pages, m.Bytes, m.SHA256 = Schema, pages, d.Bytes, d.SHA256
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dst, ManifestFile), append(raw, '\n'), 0o600); err != nil {
		return nil, err
	}
	return Load(dst)
}

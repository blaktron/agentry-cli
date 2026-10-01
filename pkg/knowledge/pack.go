// Package knowledge is the knowledge pack (plan knowledge.md §4.1,
// agentry-cli#317): an LLM wiki — compiled markdown pages with an index —
// together with the documents it was compiled from, at most 10 MB, that a
// run searches through the tools server's knowledge tools over a read-only
// /knowledge mount. Everything that makes, stores or attaches a pack reads
// the format from here: the runner (run --knowledge), the Creator's apply
// step, the local store, the desktop.
//
// A pack is one directory:
//
//	<name>/
//	  manifest.json   name, id, dates, creator, schema, pages, bytes, the
//	                  tree's sha256, the scan record
//	  wiki/
//	    index.md      every page, one line each, by section (generated)
//	    log.md        append-only: every ingest, with the documents it took
//	    <section>/<slug>.md   the pages: YAML frontmatter, [src: doc:<path>]
//	  sources/        the documents as given, and their extracted text
//
// The tree's digest is the SHA-256 of a canonical tar of wiki/ and
// sources/ (TreeDigest); it is what the receipt records.
package knowledge

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Schema is the pack format this package reads and writes.
const Schema = "knowledge/v1"

// MaxBytes caps a pack Agentry keeps or builds — every file in it, the
// manifest included: one uploaded, downloaded, shared, shipped in a release
// or made by a build In Agentry (plan D16: "no knowledge set should exceed
// 10mb"). Counted in MiB, the unit of the release archive's own cap (rule
// V8). It is also a build's cap when the caller names none.
const MaxBytes = 10 << 20

// MaxLocalBytes caps a pack made and kept on this computer, documents
// included (operator decision 2026-09-28, agentry-cli#429, DR-48): a local
// build may take up to 1 GB of documents. Such a pack past MaxBytes stays
// on this computer: Agentry takes none past MaxBytes.
const MaxLocalBytes = 1 << 30

// localCap is MaxLocalBytes, the cap Load, CopyPack and ExtractTree hold
// a pack on this computer to; a test lowers it rather than write a
// gigabyte.
var localCap int64 = MaxLocalBytes

// FormatCap is a cap as a person reads it: "10 MB", "1 GB".
func FormatCap(n int64) string {
	if n >= 1<<30 && n%(1<<30) == 0 {
		return fmt.Sprintf("%d GB", n>>30)
	}
	return fmt.Sprintf("%d MB", n>>20)
}

// maxPackName bounds a pack name, which is also a directory name.
const maxPackName = 64

// The pack's fixed entries.
const (
	ManifestFile = "manifest.json"
	WikiDir      = "wiki"
	SourcesDir   = "sources"
	IndexFile    = "index.md"
	LogFile      = "log.md"
)

var (
	// ErrTooLarge is a pack past its cap: MaxLocalBytes on this computer,
	// MaxBytes for Agentry or a build that names none.
	ErrTooLarge = errors.New("knowledge pack is past its size cap")
	// ErrDigestMismatch is a tree that no longer matches its manifest.
	ErrDigestMismatch = errors.New("knowledge pack's files do not match the digest in its manifest")
)

// Manifest is manifest.json, in the clear beside the tree it describes.
type Manifest struct {
	Schema  string    `json:"schema"`
	Name    string    `json:"name"`
	ID      string    `json:"id,omitempty"`
	Created time.Time `json:"created,omitzero"`
	Updated time.Time `json:"updated,omitzero"`
	Creator Creator   `json:"creator,omitzero"`
	// Pages counts the wiki's pages, index and log aside.
	Pages int `json:"pages"`
	// Bytes is the tree's size: every file under wiki/ and sources/.
	Bytes int64 `json:"bytes"`
	// SHA256 is TreeDigest's "sha256:<hex>".
	SHA256 string `json:"sha256"`
	Scan   Scan   `json:"scan"`
}

// Creator names what made or last extended a pack.
type Creator struct {
	CLI   string `json:"cli,omitempty"`
	Model string `json:"model,omitempty"`
}

// Scan is the pack's scan record: "not scanned" for every pack no registry
// has scanned — every pack made on a machine (plan D19) — or the record the
// registry wrote, kept as written. A record in a manifest is only a claim:
// anyone can write one into a file, so it reads as scanned only once code
// that asked the registry has set Verified, which this client's local
// paths never do.
type Scan struct {
	// Scanned is true when the manifest carries a registry's record.
	Scanned bool
	Report  json.RawMessage
	// Verified is set by code that confirmed the record with the registry
	// that wrote it; never read from or written to a file.
	Verified bool
}

const notScanned = "not scanned"

// MarshalJSON writes "not scanned" or the registry's record.
func (s Scan) MarshalJSON() ([]byte, error) {
	if !s.Scanned {
		return json.Marshal(notScanned)
	}
	if len(s.Report) == 0 {
		return []byte("{}"), nil
	}
	return s.Report, nil
}

// UnmarshalJSON reads either form; any other word is refused, so a record
// can never claim a scan by spelling.
func (s *Scan) UnmarshalJSON(b []byte) error {
	var word string
	if err := json.Unmarshal(b, &word); err == nil {
		if word != notScanned {
			return fmt.Errorf("scan record %q: want %q or the registry's record", word, notScanned)
		}
		*s = Scan{}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("scan record: %w", err)
	}
	*s = Scan{Scanned: true, Report: append(json.RawMessage(nil), b...)}
	return nil
}

// Describe is the record as a receipt and a person read it: a record no
// one here has confirmed says so rather than claiming a scan.
func (s Scan) Describe() string {
	switch {
	case !s.Scanned:
		return notScanned
	case !s.Verified:
		return "a scan record this client has not confirmed with the registry"
	}
	return "scanned"
}

// ValidName checks a pack name: 1-64 lowercase letters, digits, '.', '-'
// or '_', starting with a letter or a digit — one directory component on
// every filesystem, case-insensitive ones included.
func ValidName(name string) error {
	if name == "" || len(name) > maxPackName {
		return fmt.Errorf("knowledge pack name %q must be 1-%d characters", name, maxPackName)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '-' || r == '_'):
		default:
			return fmt.Errorf("knowledge pack name %q must be lowercase letters, digits, '.', '-' or '_', starting with a letter or digit", name)
		}
	}
	return nil
}

// Digest is a tree's digest and size.
type Digest struct {
	SHA256 string
	Bytes  int64
	Files  int
}

// TreeDigest hashes a pack's wiki/ and sources/ as a canonical tar, so that
// any implementation can reproduce it: for every regular file, in byte
// order of its slash path relative to the pack (e.g. "sources/a.txt"), a
// POSIX ustar header — name, mode "0000644", uid and gid "0000000", size
// and mtime in octal (mtime 0), typeflag '0', magic "ustar\0", version
// "00", empty linkname, uname, gname and prefix, devmajor and devminor
// "0000000", each numeric field NUL-terminated, the checksum "%06o\0 " —
// preceded by a PAX extended header only when a name needs one (longer
// than 100 bytes, or not ASCII), then the data padded to 512 bytes; after
// the last file, two zero blocks and no record padding. This is exactly
// what archive/tar writes for these headers. Anything that is not a
// directory or a regular file is refused: a pack holds files, never links.
func TreeDigest(dir string) (Digest, error) {
	return WriteTree(dir, io.Discard)
}

// WriteTree writes dir's wiki/ and sources/ to w as the canonical tar
// TreeDigest hashes — the plaintext a sealed pack holds (plan knowledge.md
// §4.3; the core's pkg/packseal seals it) — and returns its digest.
func WriteTree(dir string, w io.Writer) (Digest, error) {
	files, err := treeFiles(dir)
	if err != nil {
		return Digest{}, err
	}
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(h, w))
	var total int64
	for _, rel := range files {
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // a listed regular file of the pack
		if err != nil {
			return Digest{}, err
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return Digest{}, err
		}
		if err := tw.WriteHeader(canonicalHeader(rel, fi.Size())); err != nil {
			_ = f.Close()
			return Digest{}, err
		}
		n, err := io.Copy(tw, f)
		_ = f.Close()
		if err != nil {
			return Digest{}, err
		}
		total += n
	}
	if err := tw.Close(); err != nil {
		return Digest{}, err
	}
	return Digest{SHA256: "sha256:" + hex.EncodeToString(h.Sum(nil)), Bytes: total, Files: len(files)}, nil
}

// canonicalHeader is the one header the canonical tar gives a file.
func canonicalHeader(rel string, size int64) *tar.Header {
	return &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     rel,
		Mode:     0o644,
		Size:     size,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatPAX,
	}
}

// TreeFiles lists the regular files of the pack at dir, under wiki/ and
// sources/, as sorted slash paths relative to it: what TreeDigest hashes
// and the knowledge tools serve.
func TreeFiles(dir string) ([]string, error) { return treeFiles(dir) }

// treeFiles lists the regular files under wiki/ and sources/ as sorted
// slash paths relative to dir.
func treeFiles(dir string) ([]string, error) {
	var out []string
	for _, sub := range []string{WikiDir, SourcesDir} {
		root := filepath.Join(dir, sub)
		if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return rerr
			}
			switch {
			case d.IsDir():
				return nil
			case d.Type().IsRegular():
				out = append(out, filepath.ToSlash(rel))
				return nil
			default:
				return fmt.Errorf("%s is not a regular file; a knowledge pack holds only files", filepath.ToSlash(rel))
			}
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// Pack is a pack on disk whose manifest and tree agree.
type Pack struct {
	Dir      string
	Manifest Manifest
}

// Load reads and verifies the pack at dir: the manifest parses and names
// this schema and a valid name, the layout is the pack's (manifest.json,
// wiki/ with its index and log, sources/, nothing else), the whole pack is
// within MaxLocalBytes, and the tree's digest is the manifest's. Agentry's
// smaller cap is checked where a pack goes to Agentry (the store's upload).
func Load(dir string) (*Pack, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // the pack the caller named
	if err != nil {
		return nil, fmt.Errorf("knowledge pack %s: read %s: %w", dir, ManifestFile, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %s: %w", dir, ManifestFile, err)
	}
	if m.Schema != Schema {
		return nil, fmt.Errorf("knowledge pack %s: schema %q is not %q", dir, m.Schema, Schema)
	}
	if err := ValidName(m.Name); err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", dir, err)
	}
	if err := checkLayout(dir); err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", m.Name, err)
	}
	d, err := TreeDigest(dir)
	if err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", m.Name, err)
	}
	if size := d.Bytes + int64(len(raw)); size > localCap {
		return nil, fmt.Errorf("knowledge pack %s is %d bytes, past %s: %w", m.Name, size, FormatCap(localCap), ErrTooLarge)
	}
	if d.SHA256 != m.SHA256 {
		return nil, fmt.Errorf("knowledge pack %s: %w (manifest %s, files %s)", m.Name, ErrDigestMismatch, m.SHA256, d.SHA256)
	}
	// The counts a receipt repeats are the tree's, not the manifest's word.
	pages, err := Pages(dir)
	if err != nil {
		return nil, fmt.Errorf("knowledge pack %s: %w", m.Name, err)
	}
	if m.Pages != pages || m.Bytes != d.Bytes {
		return nil, fmt.Errorf("knowledge pack %s: manifest.json says %s and %d bytes, the files are %s and %d bytes",
			m.Name, CountPages(m.Pages), m.Bytes, CountPages(pages), d.Bytes)
	}
	return &Pack{Dir: dir, Manifest: m}, nil
}

// CountPages is a page count as a person reads it: "1 page", "3 pages".
func CountPages(n int) string { return count(n, "page") }

// CopyPack copies the pack at src to dst, which must not exist: the
// manifest and the regular files under wiki/ and sources/, nothing else —
// a link, a device or a pipe is refused, never followed — and never more
// than MaxLocalBytes in all, so a pack changed after it was checked cannot pull
// anything else into the copy. Every read goes through an os.Root at src.
// The copy is for Load to verify; CopyPack itself checks no digest.
func CopyPack(src, dst string) error {
	from, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer func() { _ = from.Close() }()
	files, err := treeFiles(src)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	to, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = to.Close() }()
	var total int64
	for _, rel := range append([]string{ManifestFile}, files...) {
		n, err := copyBetween(from, to, rel, localCap-total)
		if errors.Is(err, ErrTooLarge) {
			return fmt.Errorf("%s: the pack is past %s: %w", rel, FormatCap(localCap), err)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		total += n
	}
	return nil
}

// checkLayout holds a pack to its fixed shape.
func checkLayout(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch {
		case e.Name() == ManifestFile && e.Type().IsRegular():
		case (e.Name() == WikiDir || e.Name() == SourcesDir) && e.IsDir():
		default:
			return fmt.Errorf("%s does not belong in a knowledge pack (only %s, %s/ and %s/)", e.Name(), ManifestFile, WikiDir, SourcesDir)
		}
	}
	for _, f := range []string{IndexFile, LogFile} {
		fi, err := os.Lstat(filepath.Join(dir, WikiDir, f))
		if err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s/%s is missing", WikiDir, f)
		}
	}
	return nil
}

// Pages counts the wiki's pages under dir: every .md file under wiki/ but
// the index files and the log.
func Pages(dir string) (int, error) {
	files, err := treeFiles(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rel := range files {
		if isPage(rel) {
			n++
		}
	}
	return n, nil
}

// isPage says whether a slash path relative to the pack is a wiki page.
func isPage(rel string) bool {
	if !strings.HasPrefix(rel, WikiDir+"/") || !strings.HasSuffix(rel, ".md") {
		return false
	}
	return filepath.Base(rel) != IndexFile && rel != WikiDir+"/"+LogFile
}

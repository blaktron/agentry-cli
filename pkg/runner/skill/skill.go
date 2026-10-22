// Package skill reads Agent Skills folders: a SKILL.md whose frontmatter
// names the skill and says when to use it, and the files it uses. A
// persistent agent's skills are such folders, the built-in ones from the
// binary and the person's own from a folder or a zip on their computer.
// Every folder is held to the same bounds before anything reads it: regular
// files and folders only, no link, no name that climbs out, at most MaxFiles
// files and MaxBytes bytes.
package skill

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The bounds of a skill folder, and of the format's name and description.
const (
	MaxFiles          = 500
	MaxBytes          = 32 << 20
	MaxNameBytes      = 64
	MaxDescription    = 1024
	maxSkillMD        = 256 << 10
	maxEntryNameBytes = 255
	// maxZipBytes bounds a zip's own size before its directory is read: a
	// skill's bytes stored as they are, and room for the zip's headers.
	maxZipBytes = MaxBytes + 8<<20
)

// File is the skill's instruction file at its folder's root.
const File = "SKILL.md"

// name is the format's name: lower-case letters, digits and single
// hyphens, neither first nor last.
var name = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// NameValid says n is a skill's name: also its folder's, so it never
// climbs out of the folder that holds the skills.
func NameValid(n string) bool { return len(n) <= MaxNameBytes && name.MatchString(n) }

// DescriptionValid says d is a skill's description: text a System Prompt
// quotes, so no control or formatting character.
func DescriptionValid(d string) error {
	switch {
	case strings.TrimSpace(d) == "":
		return errors.New("a skill's description is empty")
	case len(d) > MaxDescription || !utf8.ValidString(d):
		return fmt.Errorf("a skill's description is at most %d bytes of text", MaxDescription)
	}
	for _, r := range d {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return errors.New("a skill's description holds a control or formatting character")
		}
	}
	return nil
}

// Meta is what a SKILL.md's frontmatter says of its skill.
type Meta struct {
	Name        string
	Description string
}

// Parse reads a SKILL.md's frontmatter: a YAML block between two lines of
// "---" at its start, with a name and a description. Other fields the
// format or a harness defines are left as they are.
func Parse(b []byte) (Meta, error) {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return Meta{}, errors.New(File + " does not start with its frontmatter (a line of ---)")
	}
	front, _, ok := strings.Cut(s[len("---\n"):], "\n---")
	if !ok {
		return Meta{}, errors.New(File + "'s frontmatter has no closing line of ---")
	}
	var fm map[string]any
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return Meta{}, fmt.Errorf(File+"'s frontmatter is not YAML: %w", err)
	}
	n, _ := fm["name"].(string)
	d, _ := fm["description"].(string)
	d = strings.Join(strings.Fields(d), " ")
	if !NameValid(n) {
		return Meta{}, fmt.Errorf(File+"'s name %q is not lower-case letters, digits and hyphens of at most %d bytes", n, MaxNameBytes)
	}
	if err := DescriptionValid(d); err != nil {
		return Meta{}, fmt.Errorf("%s: %w", File, err)
	}
	return Meta{Name: n, Description: d}, nil
}

// Load holds the skill folder fsys to the bounds and reads its meta.
func Load(fsys fs.FS) (Meta, error) {
	if err := check(fsys); err != nil {
		return Meta{}, err
	}
	b, err := fs.ReadFile(fsys, File)
	if err != nil {
		return Meta{}, fmt.Errorf("the skill has no %s at its root", File)
	}
	return Parse(b)
}

// check walks fsys: regular files and folders only, names that are text,
// within MaxFiles files and MaxBytes bytes.
func check(fsys fs.FS) error {
	files, total := 0, int64(0)
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." {
			if err := entryName(p); err != nil {
				return err
			}
		}
		switch {
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("the skill holds %s, which is not a file or a folder (a link is refused)", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		total += info.Size()
		switch {
		case files > MaxFiles:
			return fmt.Errorf("the skill holds more than %d files", MaxFiles)
		case total > MaxBytes:
			return fmt.Errorf("the skill is larger than %d MiB", MaxBytes>>20)
		case p == File && info.Size() > maxSkillMD:
			return fmt.Errorf("%s is larger than %d KiB", File, maxSkillMD>>10)
		}
		return nil
	})
}

// entryName refuses a path inside a skill that is not plain text: a control
// or formatting character, a backslash or a colon (a Windows stream), or a
// part past maxEntryNameBytes.
func entryName(p string) error {
	if !utf8.ValidString(p) || strings.ContainsAny(p, "\\:") {
		return fmt.Errorf("the skill holds a file whose name is not plain text: %q", p)
	}
	for _, r := range p {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("the skill holds a file whose name has a control or formatting character: %q", p)
		}
	}
	for _, part := range strings.Split(p, "/") {
		if len(part) > maxEntryNameBytes {
			return fmt.Errorf("the skill holds a name longer than %d bytes", maxEntryNameBytes)
		}
	}
	return nil
}

// CopyTo copies the skill folder fsys into dst, which must not exist: its
// files 0644, or 0755 when the source gives an executable bit, its folders
// 0755. fsys is held to the bounds first.
func CopyTo(fsys fs.FS, dst string) error {
	if err := check(fsys); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var total int64
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		if d.IsDir() {
			return root.Mkdir(p, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		src, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		out, err := root.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		n, cerr := io.Copy(out, io.LimitReader(src, MaxBytes-total+1))
		total += n
		if cerr == nil && total > MaxBytes {
			cerr = fmt.Errorf("the skill is larger than %d MiB", MaxBytes>>20)
		}
		return errors.Join(cerr, out.Close())
	})
}

// Stage takes the skill at src, a folder or a .zip on this computer, into a
// new folder under work, held to the bounds, and returns that folder and the
// skill's meta. A folder is read without following a link anywhere in it;
// a zip's entries are taken as files and folders only, beneath its root or
// its one top folder. The skill's folder is named by its SKILL.md, and a
// folder or top folder of another name is refused.
func Stage(src, work string) (string, Meta, error) {
	fi, err := os.Lstat(src)
	if err != nil {
		return "", Meta{}, err
	}
	dst, err := os.MkdirTemp(work, ".skill-*")
	if err != nil {
		return "", Meta{}, err
	}
	stage := filepath.Join(dst, "skill")
	var fsys fs.FS
	var given string
	switch {
	case fi.IsDir():
		if err := noLinks(src); err != nil {
			_ = os.RemoveAll(dst)
			return "", Meta{}, err
		}
		// Through the folder's root, so a link put in after the check is
		// not followed out of it.
		root, err := os.OpenRoot(src)
		if err != nil {
			_ = os.RemoveAll(dst)
			return "", Meta{}, err
		}
		defer func() { _ = root.Close() }()
		fsys, given = root.FS(), filepath.Base(src)
	case fi.Mode().IsRegular() && strings.EqualFold(filepath.Ext(src), ".zip"):
		zfs, top, err := openZip(src)
		if err != nil {
			_ = os.RemoveAll(dst)
			return "", Meta{}, err
		}
		fsys, given = zfs, top
	default:
		_ = os.RemoveAll(dst)
		return "", Meta{}, fmt.Errorf("%s is neither a folder nor a .zip", src)
	}
	m, err := Load(fsys)
	if err == nil && given != "" && given != m.Name {
		err = fmt.Errorf("the skill's folder is %q and its %s names it %q: the two must agree", given, File, m.Name)
	}
	if err == nil {
		err = CopyTo(fsys, stage)
	}
	if err != nil {
		_ = os.RemoveAll(dst)
		return "", Meta{}, err
	}
	return stage, m, nil
}

// noLinks refuses a folder that holds a link, or a file with another name
// elsewhere (a hard link), anywhere beneath it.
func noLinks(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("the skill holds a link, %s, which is refused", p)
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			if hardLinked(info) {
				return fmt.Errorf("the skill holds %s, a file with another name elsewhere, which is refused", p)
			}
		}
		return nil
	})
}

// openZip reads a zip into memory as a skill folder: its entries files and
// folders only, each name relative and clean, within the bounds; a zip
// whose entries are all beneath one folder is that folder, named top.
func openZip(src string) (fs.FS, string, error) {
	if fi, err := os.Stat(src); err != nil {
		return nil, "", err
	} else if fi.Size() > maxZipBytes {
		return nil, "", fmt.Errorf("the zip is larger than %d MiB", maxZipBytes>>20)
	}
	r, err := zip.OpenReader(src)
	if err != nil {
		return nil, "", fmt.Errorf("%s is not a zip: %w", src, err)
	}
	defer func() { _ = r.Close() }()
	if len(r.File) > MaxFiles*2 {
		return nil, "", fmt.Errorf("the zip holds more than %d entries", MaxFiles*2)
	}
	files := map[string][]byte{}
	var names []string
	total := int64(0)
	for _, f := range r.File {
		n := f.Name
		if strings.HasSuffix(n, "/") && f.FileInfo().IsDir() || macArchiveNoise(n) {
			continue
		}
		clean := path.Clean(n)
		switch {
		case n == "" || path.IsAbs(n) || clean != n || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(n, '\\'):
			return nil, "", fmt.Errorf("the zip holds an entry whose name climbs out or is not plain: %q", n)
		case !f.Mode().IsRegular():
			return nil, "", fmt.Errorf("the zip holds %s, which is not a file or a folder (a link is refused)", n)
		}
		if _, dup := files[n]; dup {
			return nil, "", fmt.Errorf("the zip holds %s twice", n)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, "", err
		}
		b, err := io.ReadAll(io.LimitReader(rc, MaxBytes-total+1))
		_ = rc.Close()
		if err != nil {
			return nil, "", fmt.Errorf("the zip's %s: %w", n, err)
		}
		total += int64(len(b))
		if total > MaxBytes {
			return nil, "", fmt.Errorf("the zip unpacks to more than %d MiB", MaxBytes>>20)
		}
		files[n] = b
		names = append(names, n)
	}
	// A name that is a file and a folder at once is refused: the check
	// and the copy could read it differently.
	for _, n := range names {
		for dir := path.Dir(n); dir != "."; dir = path.Dir(dir) {
			if _, ok := files[dir]; ok {
				return nil, "", fmt.Errorf("the zip holds %s as a file and as a folder", dir)
			}
		}
	}
	// One top folder holding everything is the skill's folder.
	top := ""
	if len(names) > 0 {
		first, _, ok := strings.Cut(names[0], "/")
		if ok && !slices.ContainsFunc(names, func(n string) bool { return !strings.HasPrefix(n, first+"/") }) {
			top = first
		}
	}
	m := memFS{}
	for _, n := range names {
		rel := n
		if top != "" {
			rel = strings.TrimPrefix(n, top+"/")
		}
		m[rel] = files[n]
	}
	return m, top, nil
}

// macArchiveNoise is what macOS's own zip adds beside the files: the
// resource-fork folder and Finder's view settings, never part of a skill.
func macArchiveNoise(n string) bool {
	return n == "__MACOSX" || strings.HasPrefix(n, "__MACOSX/") || path.Base(n) == ".DS_Store"
}

// Digest is a digest of the skill folder at dir: each file's path and
// bytes, in order, so two folders with the same files have the same one.
func Digest(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // a staged skill's own file
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		_, _ = fmt.Fprintf(h, "%s\x00%x\n", filepath.ToSlash(rel), sum)
		return nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// memFS is a zip's files held in memory, by slash path.
type memFS map[string][]byte

func (m memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if b, ok := m[name]; ok {
		return &memFile{name: path.Base(name), r: bytes.NewReader(b), size: int64(len(b))}, nil
	}
	var entries []fs.DirEntry
	seen := map[string]bool{}
	prefix := name + "/"
	if name == "." {
		prefix = ""
	}
	for n := range m {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		head, rest, sub := strings.Cut(strings.TrimPrefix(n, prefix), "/")
		if seen[head] {
			continue
		}
		seen[head] = true
		if sub && rest != "" {
			entries = append(entries, fs.FileInfoToDirEntry(memInfo{name: head, dir: true}))
		} else {
			entries = append(entries, fs.FileInfoToDirEntry(memInfo{name: head, size: int64(len(m[n]))}))
		}
	}
	if len(entries) == 0 && name != "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return &memDir{name: path.Base(name), entries: entries}, nil
}

type memFile struct {
	name string
	r    *bytes.Reader
	size int64
}

func (f *memFile) Stat() (fs.FileInfo, error) { return memInfo{name: f.name, size: f.size}, nil }
func (f *memFile) Read(b []byte) (int, error) { return f.r.Read(b) }
func (f *memFile) Close() error               { return nil }

// memDir is a folder of a memFS; ReadDir hands out its entries once.
type memDir struct {
	name    string
	entries []fs.DirEntry
}

func (d *memDir) Stat() (fs.FileInfo, error) { return memInfo{name: d.name, dir: true}, nil }
func (d *memDir) Read([]byte) (int, error)   { return 0, errors.New("a folder") }
func (d *memDir) Close() error               { return nil }
func (d *memDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, nil
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	k := min(n, len(d.entries))
	out := d.entries[:k]
	d.entries = d.entries[k:]
	return out, nil
}

type memInfo struct {
	name string
	size int64
	dir  bool
}

func (i memInfo) Name() string { return i.name }
func (i memInfo) Size() int64  { return i.size }
func (i memInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

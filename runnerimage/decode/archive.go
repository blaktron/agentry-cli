package decode

// Archives (plan file-handling.md §6, D5; agentry-cli#562). An archive's
// members are unpacked into the decode's own unpack directory and each is
// then decoded like a top-level file, named <archive>/<member> in the
// report. Zip and tar are read by the standard library's readers; a
// compressed stream (gzip, bzip2, xz, zstd) is decompressed by libarchive's
// bsdcat and read as a tar or as one member; 7z and rar are turned into a
// pax tar stream by libarchive's bsdtar and read the same way. So every
// member, whatever the format, passes the same checks as it streams, before
// a byte of it is written: its name, its type, the member cap shared with
// the top-level files, and the unpacked-bytes budget shared by every
// archive in the decode. Nothing an archive names reaches an argv; only
// the archive's own path does.

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Why is the closed vocabulary of why an archive or a member was not read
// in full. The host turns each into its own fixed words, so nothing an
// archive or a decoder says reaches a run.
type Why string

// The reasons.
const (
	// WhyEncrypted: the archive or the member is encrypted, and nothing
	// can check what it holds.
	WhyEncrypted Why = "encrypted"
	// WhyDamaged: the runner's own reader found it damaged, or not the
	// archive it appeared to be.
	WhyDamaged Why = "damaged"
	// WhyUnreadable: libarchive could not read it; it may be encrypted or
	// damaged, and libarchive does not always say which.
	WhyUnreadable Why = "unreadable"
	// WhyTooDeep: an archive nested deeper than the limit is not unpacked.
	WhyTooDeep Why = "too-deep"
	// WhyTooMany: unpacking stopped at the file cap.
	WhyTooMany Why = "too-many"
	// WhyTooLarge: unpacking stopped at the unpacked-bytes budget.
	WhyTooLarge Why = "too-large"
	// WhyUnsafePath: a member's name is absolute, climbs out with "..", or
	// is not a plain name (a backslash, a control or reordering mark,
	// invalid UTF-8, past the length bounds), so nothing in the archive is
	// unpacked.
	WhyUnsafePath Why = "unsafe-path"
	// WhyLink: a symbolic or hard link is never unpacked.
	WhyLink Why = "link"
	// WhySpecial: a device, a FIFO or another special file is never
	// unpacked.
	WhySpecial Why = "special"
	// WhyConflict: the archive names the path twice, or as a file and a
	// directory.
	WhyConflict Why = "conflict"
	// WhyMethod: the member is compressed by a method the runner does not
	// read.
	WhyMethod Why = "method"
)

// archiveFormat is how an archive is read.
type archiveFormat int

const (
	notArchive archiveFormat = iota
	// formatZip: the standard library's zip reader, which sees each
	// member's encryption flag and compression method.
	formatZip
	// formatTar: the standard library's tar reader.
	formatTar
	// formatStream: a compressed stream, decompressed by bsdcat, then a
	// tar or one member.
	formatStream
	// formatLibarchive: 7z and rar, turned into a pax tar stream by bsdtar.
	formatLibarchive
)

// archiveKinds are the archive types by detected MIME type. A plain zip is
// told from an Office file by Tika alone (libmagic calls both
// application/zip), so application/zip is never taken from libmagic's
// fast path (detect).
var archiveKinds = map[string]contentKind{
	"application/zip":              {family: FamilyArchive, tool: "agentry-runner", archive: formatZip},
	"application/x-tar":            {family: FamilyArchive, tool: "agentry-runner", archive: formatTar},
	"application/x-gtar":           {family: FamilyArchive, tool: "agentry-runner", archive: formatTar},
	"application/gzip":             {family: FamilyArchive, tool: "bsdcat", archive: formatStream},
	"application/x-gzip":           {family: FamilyArchive, tool: "bsdcat", archive: formatStream},
	"application/x-bzip2":          {family: FamilyArchive, tool: "bsdcat", archive: formatStream},
	"application/x-xz":             {family: FamilyArchive, tool: "bsdcat", archive: formatStream},
	"application/zstd":             {family: FamilyArchive, tool: "bsdcat", archive: formatStream},
	"application/x-7z-compressed":  {family: FamilyArchive, tool: "bsdtar", archive: formatLibarchive},
	"application/vnd.rar":          {family: FamilyArchive, tool: "bsdtar", archive: formatLibarchive},
	"application/x-rar-compressed": {family: FamilyArchive, tool: "bsdtar", archive: formatLibarchive},
}

// The bounds on a member's name.
const (
	maxMemberName = 1024
	maxNamePart   = 255
)

// PlainMemberPath says whether a member's path, as a report names it
// below its archive, is one the decode could have written: already clean
// and within memberName's rules. The host holds a report to it, since the
// container that wrote the report read the archive.
func PlainMemberPath(p string) bool {
	name, ok := memberName(p)
	return ok && name == p && name != "."
}

// memberName is the member's name as the run will see it, cleaned, or
// false when it is not a plain relative name: absolute, with a ".."
// part, a backslash, a control character, invalid UTF-8 or past the
// length bounds. "." is the archive's own root, which is skipped.
func memberName(raw string) (string, bool) {
	if raw == "" || len(raw) > maxMemberName || !utf8.ValidString(raw) || strings.HasPrefix(raw, "/") || strings.Contains(raw, "\\") {
		return "", false
	}
	for _, r := range raw {
		// C0 and C1 controls, and the marks that reorder or hide text,
		// which would let a name forge a row of list_inputs or read as
		// another.
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff {
			return "", false
		}
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." || len(part) > maxNamePart {
			return "", false
		}
	}
	return path.Clean(raw), true
}

// member is one thing an archive holds, as unpacked: a file written under
// the archive's unpack directory, or one that was not, with why.
type member struct {
	name string
	size int64
	why  Why
}

// unpacker writes one archive's members under its own root, against the
// decode's shared caps.
type unpacker struct {
	d       *decoder
	root    *os.Root
	members []member
	// named finds a member by name: an archive that names a path twice
	// has neither read, under one entry.
	named map[string]int
	// stop is why unpacking stopped before the archive's end, if it did.
	stop Why
	// pool is the cap the archive's members count against (poolOf).
	pool string
}

// record adds a member under its name, or marks the one already there a
// conflict and removes what was written for it.
func (u *unpacker) record(m member) {
	if i, dup := u.named[m.name]; dup {
		if u.members[i].why == "" {
			_ = u.root.Remove(m.name)
		}
		u.members[i] = member{name: m.name, why: WhyConflict}
		return
	}
	u.named[m.name] = len(u.members)
	u.members = append(u.members, m)
}

// add records a member that is not written, counting it against the cap.
// It answers false when the cap was reached, and unpacking stops.
func (u *unpacker) add(name string, why Why) bool {
	if !u.count() {
		return false
	}
	u.record(member{name: name, why: why})
	return true
}

// count takes one member from the file cap, or stops at it.
func (u *unpacker) count() bool {
	if !u.d.take(u.pool) {
		u.stop = WhyTooMany
		return false
	}
	return true
}

// dir makes a directory the archive names; a clash is left to the members
// under it, which then conflict.
func (u *unpacker) dir(name string) {
	if name != "." {
		_ = u.root.MkdirAll(name, 0o755)
	}
}

// file writes one member from r, as it streams, within the budget. It
// answers false when unpacking must stop: the cap, or the budget.
func (u *unpacker) file(name string, r io.Reader) bool {
	if name == "." {
		return true
	}
	if !u.count() {
		return false
	}
	if _, dup := u.named[name]; dup {
		u.record(member{name: name})
		return true
	}
	if dir := path.Dir(name); dir != "." {
		if err := u.root.MkdirAll(dir, 0o755); err != nil {
			u.record(member{name: name, why: WhyConflict})
			return true
		}
	}
	f, err := u.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		u.record(member{name: name, why: WhyConflict})
		return true
	}
	n, err := io.Copy(f, io.LimitReader(r, u.d.budget+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	// A member that would pass the budget is removed, so its bytes are
	// not counted: it stops its own archive, not the archives after it.
	if n > u.d.budget || isNoSpace(err) {
		_ = u.root.Remove(name)
		u.stop = WhyTooLarge
		return false
	}
	u.d.budget -= n
	if err != nil {
		_ = u.root.Remove(name)
		u.record(member{name: name, why: WhyDamaged})
		return true
	}
	u.record(member{name: name, size: n})
	return true
}

// isNoSpace says the unpack tmpfs is full: the kernel's own bound on what
// the budget counts.
func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// unpackInto reads the archive at src into dir. It answers what it unpacked,
// why it stopped early (empty when it read to the end), and whether the
// archive could be opened at all. A member with an unsafe name refuses the
// whole archive: no member of it is answered.
func (d *decoder) unpackInto(ctx context.Context, src, dir, rel string, kind contentKind) ([]member, Why, bool) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, WhyDamaged, false
	}
	defer func() { _ = root.Close() }()
	u := &unpacker{d: d, root: root, named: map[string]int{}, pool: d.poolOf(rel)}
	if d.seen == nil {
		d.seen = map[string]int{}
	}
	seen := d.seen[u.pool]
	var why Why
	switch kind.archive {
	case formatZip:
		why = u.zip(src)
	case formatTar:
		why = u.tarFile(src)
	case formatStream:
		why = u.stream(ctx, src, rel)
	case formatLibarchive:
		why = u.libarchive(ctx, src)
	}
	if why == WhyUnsafePath {
		d.seen[u.pool] = seen
		return nil, WhyUnsafePath, false
	}
	if u.stop != "" {
		why = u.stop
	}
	if why != "" && len(u.members) == 0 && why != WhyTooMany && why != WhyTooLarge {
		return nil, why, false
	}
	return u.members, why, true
}

// The zip format's general-purpose flag for an encrypted member, and its
// method number for bzip2, which the standard library reads.
const (
	zipFlagEncrypted = 0x1
	zipMethodBzip2   = 12
)

// zip reads a zip with the standard library: every name is checked before
// anything is written, and each member's encryption flag and method are
// seen before its data is read.
func (u *unpacker) zip(src string) Why {
	zr, err := zip.OpenReader(src)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return WhyDamaged
	}
	defer func() { _ = zr.Close() }()
	zr.RegisterDecompressor(zipMethodBzip2, func(r io.Reader) io.ReadCloser { return io.NopCloser(bzip2.NewReader(r)) })
	names := make([]string, len(zr.File))
	for i, zf := range zr.File {
		name, ok := memberName(zf.Name)
		if !ok {
			return WhyUnsafePath
		}
		names[i] = name
	}
	for i, zf := range zr.File {
		name, mode := names[i], zf.Mode()
		ok := true
		switch {
		case mode.IsDir():
			u.dir(name)
		case mode&fs.ModeSymlink != 0:
			ok = u.add(name, WhyLink)
		case !mode.IsRegular():
			ok = u.add(name, WhySpecial)
		case zf.Flags&zipFlagEncrypted != 0:
			ok = u.add(name, WhyEncrypted)
		case zf.Method != zip.Store && zf.Method != zip.Deflate && zf.Method != zipMethodBzip2:
			ok = u.add(name, WhyMethod)
		default:
			rc, err := zf.Open()
			if err != nil {
				ok = u.add(name, WhyDamaged)
				break
			}
			ok = u.file(name, rc)
			_ = rc.Close()
		}
		if !ok {
			return ""
		}
	}
	return ""
}

// tarFile reads a tar file with the standard library.
func (u *unpacker) tarFile(src string) Why {
	f, err := os.Open(src) //nolint:gosec // an input under the root being decoded, or a member this decode unpacked
	if err != nil {
		return WhyDamaged
	}
	defer func() { _ = f.Close() }()
	return u.tar(tar.NewReader(f), WhyDamaged)
}

// tar reads a tar stream member by member, checking each header before its
// data. A stream that fails partway answers onError.
func (u *unpacker) tar(tr *tar.Reader, onError Why) Why {
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return ""
		}
		if err != nil && !errors.Is(err, tar.ErrInsecurePath) {
			return onError
		}
		name, ok := memberName(h.Name)
		if !ok {
			return WhyUnsafePath
		}
		switch h.Typeflag {
		case tar.TypeDir:
			u.dir(name)
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is what old tars write
			ok = u.file(name, tr)
		case tar.TypeSymlink, tar.TypeLink:
			ok = u.add(name, WhyLink)
		default:
			ok = u.add(name, WhySpecial)
		}
		if !ok {
			return ""
		}
	}
}

// streamSuffixes are the compressed-stream suffixes a lone member's name
// drops: report.docx.gz holds report.docx.
var streamSuffixes = []string{".gz", ".bz2", ".xz", ".zst"}

// stream decompresses a compressed stream with bsdcat: a tar inside is
// read as one; anything else is one member, named for the stream less its
// suffix.
func (u *unpacker) stream(ctx context.Context, src, rel string) Why {
	cmd := decoderCommand(ctx, u.d.tools.Bsdcat, src)
	lone := false
	why := u.fromProcess(cmd, WhyDamaged, func(r *bufio.Reader) Why {
		head, _ := r.Peek(512)
		if len(head) >= 262 && string(head[257:262]) == "ustar" {
			return u.tar(tar.NewReader(r), WhyDamaged)
		}
		lone = true
		u.file(loneName(rel), r)
		return ""
	})
	// A stream that fails has cut its one member short, without a reader
	// that could tell: it is not kept.
	if lone && why != "" && u.stop == "" {
		for _, m := range u.members {
			_ = u.root.Remove(m.name)
			u.d.seen[u.pool]--
		}
		u.members = nil
	}
	return why
}

// loneName is the one member a compressed stream holds.
func loneName(rel string) string {
	base := path.Base(rel)
	for _, s := range streamSuffixes {
		if trimmed, ok := strings.CutSuffix(base, s); ok && trimmed != "" {
			if name, ok := memberName(trimmed); ok && name != "." {
				return name
			}
		}
	}
	return "contents"
}

// libarchive has bsdtar write a 7z or a rar as a pax tar stream, which is
// read like any other. bsdtar's words go no further than telling an
// encrypted archive from one it could not read.
func (u *unpacker) libarchive(ctx context.Context, src string) Why {
	// -P keeps each name as the archive gives it: without it bsdtar
	// strips a leading "/", and the name check would never see what the
	// archive tried.
	cmd := decoderCommand(ctx, u.d.tools.Bsdtar, "-c", "-f", "-", "--format", "pax", "-P", "@"+src)
	return u.fromProcess(cmd, WhyUnreadable, func(r *bufio.Reader) Why {
		return u.tar(tar.NewReader(r), WhyUnreadable)
	})
}

// trailingBytes is what may follow a tar stream's end: bsdtar pads its
// output to a 10,240-byte block, and a compressed tar may carry a little
// more.
const trailingBytes = 1 << 20

// fromProcess runs a decompressor and reads its output with read. A
// process that fails answers onError, or WhyEncrypted when it says the
// archive is encrypted; one stopped early by the caps is not a failure.
func (u *unpacker) fromProcess(cmd *exec.Cmd, onError Why, read func(*bufio.Reader) Why) Why {
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 1024}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return onError
	}
	if err := cmd.Start(); err != nil {
		return onError
	}
	why := read(bufio.NewReaderSize(out, 64<<10))
	stopped := why == WhyUnsafePath || u.stop != ""
	// Whatever ended the read early, the rest of the stream is not read:
	// a bomb behind a bad header is not decompressed to the end. After a
	// tar's end only its padding should follow; more than trailingBytes
	// is not decompressed to the end either, and the archive is damaged.
	if why == "" && !stopped {
		if n, _ := io.Copy(io.Discard, io.LimitReader(out, trailingBytes+1)); n > trailingBytes {
			why = WhyDamaged
		}
	}
	if why != "" || stopped {
		_ = cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait()
	switch {
	case stopped:
		return why
	case err != nil || why != "":
		if msg := strings.ToLower(stderr.String()); strings.Contains(msg, "encrypt") || strings.Contains(msg, "passphrase") {
			return WhyEncrypted
		}
		return onError
	}
	return ""
}

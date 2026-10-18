// Package beneath holds a name beneath a directory: every open, make,
// remove, rename and ownership change resolves its path from a directory
// the caller opened, and a path that would leave it is refused.
//
// On Linux a Dir resolves each path with a single openat2(2) call carrying
// RESOLVE_BENEATH, so the kernel (or gVisor's equivalent) refuses a symlink
// that leads out of the directory at the moment of use, wherever in the path
// it sits. os.Root walks a path one directory at a time with openat(2) and
// O_NOFOLLOW, which gVisor does not apply to a symlinked directory, so an
// os.Root there opens through a link that was swapped in and out of its
// root. A Dir does not depend on that behaviour.
//
// Where openat2 is missing (a kernel before 5.6, or a seccomp profile that
// predates it) and on every other operating system a Dir is an os.Root.
//
// The package imports only the standard library: the runner image compiles
// it from its embedded sources with no module downloads.
package beneath

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

// ErrEscapes is the error of a path that leaves the directory it is
// resolved beneath: a ".." or an absolute path, or a symlink that leads out.
var ErrEscapes = errors.New("path escapes from parent")

// ErrLink is the error of OpenNoLinks for a path with a symlink in it.
var ErrLink = errors.New("path passes through a symlink")

// Dir is a directory that names are resolved beneath. It is safe for
// concurrent use; Close waits for the calls in progress.
type Dir struct {
	impl impl
}

// impl is one way of holding names beneath a directory.
type impl interface {
	open(rel string, flag int, perm fs.FileMode) (*os.File, error)
	openNoLinks(rel string) (*os.File, error)
	mkdir(rel string, perm fs.FileMode) error
	chmod(rel string, mode fs.FileMode) error
	remove(rel string) error
	rename(oldRel, newRel string) error
	lchown(rel string, uid, gid int) error
	close() error
	// beneath says whether paths are resolved by openat2.
	beneath() bool
}

// Open opens the directory name and returns a Dir on it. name itself is
// resolved as the operating system resolves any path; every name used
// through the Dir is held beneath it.
func Open(name string) (*Dir, error) {
	if d, ok, err := openFD(name); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return &Dir{impl: d}, nil
	}
	r, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return &Dir{impl: rootImpl{r}}, nil
}

// Beneath says whether this Dir resolves paths with openat2 rather than an
// os.Root.
func (d *Dir) Beneath() bool { return d.impl.beneath() }

// Close closes the directory.
func (d *Dir) Close() error { return d.impl.close() }

// Open opens the file or directory rel for reading. A symlink in rel is
// followed only while it stays beneath the Dir.
func (d *Dir) Open(rel string) (*os.File, error) {
	return d.impl.open(rel, os.O_RDONLY, 0)
}

// OpenFile is os.OpenFile of rel beneath the Dir.
func (d *Dir) OpenFile(rel string, flag int, perm fs.FileMode) (*os.File, error) {
	return d.impl.open(rel, flag, perm)
}

// OpenNoLinks opens rel for reading, refusing a symlink anywhere in it,
// the last element included, with ErrLink. A file that is not a regular one
// is opened without blocking; it is for the caller to look at.
func (d *Dir) OpenNoLinks(rel string) (*os.File, error) {
	return d.impl.openNoLinks(rel)
}

// ErrNotRegular is the error of CreateRegular for a name that is a link, a
// directory or a pipe.
var ErrNotRegular = errors.New("not a regular file")

// CreateRegular opens rel for writing, creating it with perm when it is not
// there, and empties it. A link, a directory or a pipe in its place is
// refused with ErrNotRegular: nothing is written through a link or waited
// for.
func (d *Dir) CreateRegular(rel string, perm fs.FileMode) (*os.File, error) {
	refused := &fs.PathError{Op: "open", Path: rel, Err: ErrNotRegular}
	// An os.Root follows a link that stays beneath it whatever the flags
	// say, so it is looked at first.
	if l, ok := d.impl.(lstater); ok {
		if fi, err := l.lstat(rel); err == nil && !fi.Mode().IsRegular() {
			return nil, refused
		}
	}
	f, err := d.impl.open(rel, os.O_WRONLY|os.O_CREATE|noFollow|nonBlock, perm)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EISDIR) || errors.Is(err, syscall.ENXIO) {
			return nil, refused
		}
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, refused
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Stat describes rel, following a symlink only while it stays beneath the
// Dir. It opens the name without blocking, so a pipe is described, not
// waited for.
func (d *Dir) Stat(rel string) (fs.FileInfo, error) {
	f, err := d.impl.open(rel, os.O_RDONLY|nonBlock, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Stat()
}

// Mkdir makes the directory rel, whose parent must be beneath the Dir.
func (d *Dir) Mkdir(rel string, perm fs.FileMode) error { return d.impl.mkdir(rel, perm) }

// Chmod changes the mode of rel, which must be readable.
func (d *Dir) Chmod(rel string, mode fs.FileMode) error { return d.impl.chmod(rel, mode) }

// Remove removes the file or the empty directory rel, a symlink itself and
// not what it points to.
func (d *Dir) Remove(rel string) error { return d.impl.remove(rel) }

// Rename renames oldRel to newRel, both beneath the Dir.
func (d *Dir) Rename(oldRel, newRel string) error { return d.impl.rename(oldRel, newRel) }

// Lchown changes the owner of rel, a symlink itself and not what it points
// to.
func (d *Dir) Lchown(rel string, uid, gid int) error { return d.impl.lchown(rel, uid, gid) }

// FS is the Dir as an fs.FS: a read-only view whose every open is held
// beneath the Dir.
func (d *Dir) FS() fs.FS { return dirFS{d} }

type dirFS struct{ d *Dir }

func (f dirFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	file, err := f.d.Open(name)
	if err != nil {
		return nil, err
	}
	return file, nil
}

// lstater is an impl that can describe a name without following a link.
type lstater interface {
	lstat(rel string) (fs.FileInfo, error)
}

// rootImpl holds names with an os.Root.
type rootImpl struct{ r *os.Root }

func (r rootImpl) open(rel string, flag int, perm fs.FileMode) (*os.File, error) {
	return r.r.OpenFile(rel, flag, perm)
}

func (r rootImpl) openNoLinks(rel string) (*os.File, error) {
	rel = path.Clean(rel)
	var last fs.FileInfo
	parts := strings.Split(rel, "/")
	for i := range parts {
		fi, err := r.r.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, &fs.PathError{Op: "open", Path: rel, Err: ErrLink}
		}
		last = fi
	}
	f, err := r.r.OpenFile(rel, os.O_RDONLY|nonBlock, 0)
	if err != nil {
		return nil, err
	}
	// The file opened is the one looked at.
	if fi, err := f.Stat(); err != nil || !os.SameFile(fi, last) {
		_ = f.Close()
		return nil, &fs.PathError{Op: "open", Path: rel, Err: errChanged}
	}
	return f, nil
}

var errChanged = errors.New("changed while it was opened")

func (r rootImpl) lstat(rel string) (fs.FileInfo, error)    { return r.r.Lstat(rel) }
func (r rootImpl) mkdir(rel string, perm fs.FileMode) error { return r.r.Mkdir(rel, perm) }
func (r rootImpl) chmod(rel string, mode fs.FileMode) error { return r.r.Chmod(rel, mode) }
func (r rootImpl) remove(rel string) error                  { return r.r.Remove(rel) }
func (r rootImpl) rename(oldRel, newRel string) error       { return r.r.Rename(oldRel, newRel) }
func (r rootImpl) lchown(rel string, uid, gid int) error    { return r.r.Lchown(rel, uid, gid) }
func (r rootImpl) close() error                             { return r.r.Close() }
func (r rootImpl) beneath() bool                            { return false }

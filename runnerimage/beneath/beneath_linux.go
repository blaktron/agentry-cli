//go:build linux

package beneath

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

const (
	// sysOpenat2 is the number of openat2(2), the same on every Linux
	// architecture (the syscall package does not name it).
	sysOpenat2 = 437

	resolveNoMagicLinks = 0x02
	resolveNoSymlinks   = 0x04
	resolveBeneath      = 0x08

	atFDCWD           = -100
	atRemoveDir       = 0x200
	atSymlinkNoFollow = 0x100
)

// openHow is struct open_how of openat2(2).
type openHow struct {
	flags   uint64
	mode    uint64
	resolve uint64
}

// openat2 resolves path from dirfd under how.resolve and opens it. A call
// the kernel could not complete safely (EAGAIN: a rename raced a ".."
// walk) or a signal interrupted is retried.
func openat2(dirfd int, p string, flags int, mode uint32, resolve uint64) (int, error) {
	pp, err := syscall.BytePtrFromString(p)
	if err != nil {
		return -1, err
	}
	how := openHow{flags: uint64(flags) | syscall.O_CLOEXEC, mode: uint64(mode), resolve: resolve}
	for range 64 {
		fd, _, errno := syscall.Syscall6(sysOpenat2, uintptr(dirfd), uintptr(unsafe.Pointer(pp)),
			uintptr(unsafe.Pointer(&how)), unsafe.Sizeof(how), 0, 0)
		switch errno {
		case 0:
			return int(fd), nil
		case syscall.EINTR, syscall.EAGAIN:
			continue
		default:
			return -1, errno
		}
	}
	return -1, syscall.EAGAIN
}

// disabled turns openat2 off, so a test can exercise the os.Root fallback.
var disabled bool

// usable says whether this kernel, under this container runtime and seccomp
// profile, answers openat2 with RESOLVE_BENEATH. ENOSYS is a kernel before
// 5.6; EPERM is a seccomp profile that predates the call. Either leaves the
// os.Root fallback; any other answer is not trusted either.
var usable = sync.OnceValue(func() bool {
	fd, err := openat2(atFDCWD, ".", syscall.O_RDONLY|syscall.O_DIRECTORY, 0, resolveBeneath)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	return true
})

// openFD opens the directory name for resolving paths with openat2. ok is
// false when openat2 is not to be used, and the caller falls back.
func openFD(name string) (impl, bool, error) {
	if disabled || !usable() {
		return nil, false, nil
	}
	var fd int
	var err error
	for {
		fd, err = syscall.Open(name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return nil, false, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		abs = name
	}
	return &fdImpl{fd: fd, name: abs}, true, nil
}

// fdImpl holds names beneath the directory fd with openat2.
type fdImpl struct {
	mu sync.RWMutex
	fd int
	// name is the directory's path, which the files opened beneath it
	// are named under, as an os.Root names them: a listing's entries ask
	// for their sizes by it.
	name   string
	closed bool
}

var errClosed = errors.New("directory already closed")

func (d *fdImpl) beneath() bool { return true }

func (d *fdImpl) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return syscall.Close(d.fd)
}

// failed is err as the caller sees it for op on rel: a path out of the
// directory is ErrEscapes, as an os.Root says it.
func failed(op, rel string, err error) error {
	if errors.Is(err, syscall.EXDEV) {
		err = ErrEscapes
	}
	return &fs.PathError{Op: op, Path: rel, Err: err}
}

func (d *fdImpl) openat2(op, rel string, flags int, mode uint32, resolve uint64) (*os.File, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, &fs.PathError{Op: op, Path: rel, Err: errClosed}
	}
	fd, err := openat2(d.fd, rel, flags, mode, resolve)
	if err != nil {
		return nil, failed(op, rel, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(d.name, rel)), nil
}

func (d *fdImpl) open(rel string, flag int, perm fs.FileMode) (*os.File, error) {
	return d.openat2("openat", rel, flag, syscallMode(perm), resolveBeneath|resolveNoMagicLinks)
}

func (d *fdImpl) openNoLinks(rel string) (*os.File, error) {
	f, err := d.openat2("openat", rel, syscall.O_RDONLY|syscall.O_NONBLOCK, 0,
		resolveBeneath|resolveNoSymlinks|resolveNoMagicLinks)
	if err != nil && errors.Is(err, syscall.ELOOP) {
		return nil, &fs.PathError{Op: "openat", Path: rel, Err: ErrLink}
	}
	return f, err
}

// syscallMode is os's conversion of a FileMode to the bits of a call.
func syscallMode(m fs.FileMode) uint32 {
	o := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		o |= syscall.S_ISUID
	}
	if m&fs.ModeSetgid != 0 {
		o |= syscall.S_ISGID
	}
	if m&fs.ModeSticky != 0 {
		o |= syscall.S_ISVTX
	}
	return o
}

// leaf opens the directory that holds rel's last element, beneath the
// directory, and returns it with that element. An operation on the one
// element then acts on that name in that directory and on no path, so there
// is nothing left to swap. done releases the parent.
func (d *fdImpl) leaf(op, rel string) (parent int, name string, done func(), err error) {
	clean := path.Clean(rel)
	if path.IsAbs(clean) {
		return 0, "", nil, failed(op, rel, ErrEscapes)
	}
	dir, name := path.Split(clean)
	if name == "" || name == "." || name == ".." {
		return 0, "", nil, failed(op, rel, syscall.EINVAL)
	}
	if dir == "" {
		return d.fd, name, func() {}, nil
	}
	fd, err := openat2(d.fd, path.Clean(dir), syscall.O_RDONLY|syscall.O_DIRECTORY, 0, resolveBeneath|resolveNoMagicLinks)
	if err != nil {
		return 0, "", nil, failed(op, rel, err)
	}
	return fd, name, func() { _ = syscall.Close(fd) }, nil
}

func (d *fdImpl) mkdir(rel string, perm fs.FileMode) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return &fs.PathError{Op: "mkdirat", Path: rel, Err: errClosed}
	}
	parent, name, done, err := d.leaf("mkdirat", rel)
	if err != nil {
		return err
	}
	defer done()
	if err := retry(func() error { return syscall.Mkdirat(parent, name, syscallMode(perm)) }); err != nil {
		return failed("mkdirat", rel, err)
	}
	return nil
}

func (d *fdImpl) remove(rel string) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return &fs.PathError{Op: "unlinkat", Path: rel, Err: errClosed}
	}
	parent, name, done, err := d.leaf("unlinkat", rel)
	if err != nil {
		return err
	}
	defer done()
	first := retry(func() error { return unlinkat(parent, name, 0) })
	if first == nil {
		return nil
	}
	second := retry(func() error { return unlinkat(parent, name, atRemoveDir) })
	if second == nil {
		return nil
	}
	if second == syscall.ENOTDIR {
		second = first // not a directory: the first answer is the true one
	}
	return failed("unlinkat", rel, second)
}

func (d *fdImpl) rename(oldRel, newRel string) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return &fs.PathError{Op: "renameat", Path: oldRel, Err: errClosed}
	}
	oldParent, oldName, oldDone, err := d.leaf("renameat", oldRel)
	if err != nil {
		return err
	}
	defer oldDone()
	newParent, newName, newDone, err := d.leaf("renameat", newRel)
	if err != nil {
		return err
	}
	defer newDone()
	if err := retry(func() error { return syscall.Renameat(oldParent, oldName, newParent, newName) }); err != nil {
		return &os.LinkError{Op: "renameat", Old: oldRel, New: newRel, Err: errOf(err)}
	}
	return nil
}

func (d *fdImpl) lchown(rel string, uid, gid int) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return &fs.PathError{Op: "fchownat", Path: rel, Err: errClosed}
	}
	parent, name, done, err := d.leaf("fchownat", rel)
	if err != nil {
		return err
	}
	defer done()
	if err := retry(func() error { return syscall.Fchownat(parent, name, uid, gid, atSymlinkNoFollow) }); err != nil {
		return failed("fchownat", rel, err)
	}
	return nil
}

// chmod changes the mode through the open file, so the file changed is the
// one opened beneath the directory.
func (d *fdImpl) chmod(rel string, mode fs.FileMode) error {
	f, err := d.open(rel, os.O_RDONLY|nonBlock, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Chmod(mode)
}

// unlinkat is unlinkat(2) with its flags, which the syscall package's
// Unlinkat does not take.
func unlinkat(dirfd int, name string, flags int) error {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(flags)); errno != 0 {
		return errno
	}
	return nil
}

// retry runs call again while a signal interrupts it.
func retry(call func() error) error {
	for {
		if err := call(); err != syscall.EINTR {
			return err
		}
	}
}

func errOf(err error) error {
	if errors.Is(err, syscall.EXDEV) {
		return ErrEscapes
	}
	return err
}

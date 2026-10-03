//go:build linux

package confine

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Landlock's syscall numbers are the same on every architecture (they were
// added after the tables were unified), and so is prctl's option.
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	createRulesetVersion = 1 << 0
	rulePathBeneath      = 1
	prSetNoNewPrivs      = 38
	// oPath is O_PATH, which the syscall package does not export on every
	// architecture: a descriptor that names a file without opening it.
	oPath = 0x200000
)

// The filesystem access rights that write (LANDLOCK_ACCESS_FS_*), by the
// ABI that introduced them. Reading and executing are left unhandled, so
// Landlock does not restrict them.
const (
	accessWriteFile  = 1 << 1
	accessRemoveDir  = 1 << 4
	accessRemoveFile = 1 << 5
	accessMakeChar   = 1 << 6
	accessMakeDir    = 1 << 7
	accessMakeReg    = 1 << 8
	accessMakeSock   = 1 << 9
	accessMakeFifo   = 1 << 10
	accessMakeBlock  = 1 << 11
	accessMakeSym    = 1 << 12
	accessRefer      = 1 << 13 // ABI 2: link or rename into another directory
	accessTruncate   = 1 << 14 // ABI 3

	writesV1 = accessWriteFile | accessRemoveDir | accessRemoveFile | accessMakeChar | accessMakeDir |
		accessMakeReg | accessMakeSock | accessMakeFifo | accessMakeBlock | accessMakeSym
)

// rulesetAttr is struct landlock_ruleset_attr's first field; the kernel
// accepts the shorter struct of an older ABI.
type rulesetAttr struct {
	handledAccessFS uint64
}

// pathBeneathAttr is struct landlock_path_beneath_attr, which the kernel
// declares packed: 12 bytes, which Go's layout matches before its padding.
type pathBeneathAttr struct {
	allowedAccess uint64
	parentFd      int32
}

// devNull is the one file outside the writable directories a command may
// write: plenty of programs send output to it.
const devNull = "/dev/null"

// ABI is the running kernel's Landlock ABI version, or why there is none:
// ENOSYS on a kernel or runtime without it (gVisor), EOPNOTSUPP when the
// LSM is built but not enabled, EPERM when a seccomp filter refuses it.
func ABI() (int, error) {
	v, _, errno := syscall.RawSyscall(sysLandlockCreateRuleset, 0, 0, createRulesetVersion)
	if errno != 0 {
		return 0, fmt.Errorf("landlock: %w", errno)
	}
	return int(v), nil
}

// writeRights is every write right abi handles, and fileRights the ones a
// rule on a single file (not a directory) may grant.
func writeRights(abi int) (all, file uint64) {
	all, file = writesV1, accessWriteFile
	if abi >= 2 {
		all |= accessRefer
	}
	if abi >= 3 {
		all |= accessTruncate
		file |= accessTruncate
	}
	return all, file
}

// Exec confines this process to writing beneath dirs (and to /dev/null),
// then replaces it with path, argv and env. It returns only on failure.
func Exec(dirs []string, path string, argv, env []string) error {
	abi, err := ABI()
	if err != nil {
		return err
	}
	all, file := writeRights(abi)
	attr := rulesetAttr{handledAccessFS: all}
	fd, _, errno := syscall.RawSyscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock: create the ruleset: %w", errno)
	}
	defer func() { _ = syscall.Close(int(fd)) }()
	if err := addRule(int(fd), devNull, file); err != nil {
		return err
	}
	for _, d := range dirs {
		if err := addRule(int(fd), d, all); err != nil {
			return err
		}
	}
	// The failure report must not reach the command (ErrorFD); EBADF only
	// says the caller passed none.
	_, _, _ = syscall.RawSyscall(syscall.SYS_FCNTL, ErrorFD, syscall.F_SETFD, syscall.FD_CLOEXEC)
	// Landlock and no_new_privs bind the calling thread, and execve
	// starts the program from that thread, so all three happen on one.
	runtime.LockOSThread()
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock: set no_new_privs: %w", errno)
	}
	if _, _, errno := syscall.RawSyscall(sysLandlockRestrictSelf, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock: restrict: %w", errno)
	}
	if err := syscall.Exec(path, argv, env); err != nil {
		return fmt.Errorf("confine: exec %s: %w", path, err)
	}
	return nil
}

// addRule lets the ruleset's process write beneath path with rights.
func addRule(ruleset int, path string, rights uint64) error {
	pfd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("landlock: open %s: %w", path, err)
	}
	defer func() { _ = syscall.Close(pfd) }()
	rule := pathBeneathAttr{allowedAccess: rights, parentFd: int32(pfd)} //nolint:gosec // a descriptor fits in an int32
	if _, _, errno := syscall.RawSyscall6(sysLandlockAddRule, uintptr(ruleset), rulePathBeneath, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock: allow writes beneath %s: %w", path, errno)
	}
	return nil
}

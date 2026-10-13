//go:build linux

package confine

import (
	"fmt"
	"runtime"
	"runtime/debug"
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
// ABI that introduced them. Reading and executing are handled only when the
// caller names what may be read (Options.Readable); otherwise Landlock does
// not restrict them.
const (
	accessExecute  = 1 << 0
	accessReadFile = 1 << 2
	accessReadDir  = 1 << 3
	readsV1        = accessExecute | accessReadFile | accessReadDir

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

// The network access rights (LANDLOCK_ACCESS_NET_*, ABI 4): a ruleset that
// handles them and grants no port refuses every TCP bind and connect.
const (
	accessNetBindTCP    = 1 << 0
	accessNetConnectTCP = 1 << 1
	netABI              = 4
)

// rulesetAttr is struct landlock_ruleset_attr. The kernel takes the
// shorter struct of an older ABI, so only a ruleset that handles the
// network passes the second field.
type rulesetAttr struct {
	handledAccessFS  uint64
	handledAccessNet uint64
}

// NetworkDenied says whether NoNetwork refuses TCP at Landlock ABI abi.
func NetworkDenied(abi int) bool { return abi >= netABI }

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

// ExecWith holds this process to o, then replaces it with path, argv and
// env. It returns only on failure.
//
// The address-space limit comes last, after every argument of execve is
// built: this process is Go, whose runtime may already hold more address
// space than the limit, so nothing may allocate once it is set. A failed
// execve past that point is reported and exits without allocating
// (failExec).
func ExecWith(o Options, path string, argv, env []string) error {
	argv0p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return fmt.Errorf("confine: exec %s: %w", path, err)
	}
	argvp, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return fmt.Errorf("confine: exec %s: %w", path, err)
	}
	envvp, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return fmt.Errorf("confine: exec %s: %w", path, err)
	}
	// The failure report must not reach the command (ErrorFD); EBADF only
	// says the caller passed none.
	_, _, _ = syscall.RawSyscall(syscall.SYS_FCNTL, ErrorFD, syscall.F_SETFD, syscall.FD_CLOEXEC)
	// Landlock and no_new_privs bind the calling thread, and execve
	// starts the program from that thread, so all of it happens on one.
	runtime.LockOSThread()
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("confine: set no_new_privs: %w", errno)
	}
	o.program = path
	if !o.LimitsOnly {
		if err := restrict(o); err != nil {
			return err
		}
	}
	if o.MaxMemory == 0 {
		_, _, errno := syscall.RawSyscall(syscall.SYS_EXECVE, uintptr(unsafe.Pointer(argv0p)), uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envvp[0])))
		return fmt.Errorf("confine: exec %s: %w", path, errno)
	}
	// From here on, no allocation: no collection either, and no other
	// goroutine running. The limit binds every thread of the process, and
	// a goroutine on another P that needs memory between the limit and the
	// execve gets none: the runtime dies of it ("fatal error: runtime:
	// cannot allocate memory"), on the program's stderr when the execve
	// wins and in its place when it does not (agentry-cli#887; one run in
	// a hundred on a loaded machine). With one P, held by this goroutine
	// through the raw syscalls below, which never give it up, nothing else
	// runs until the program replaces the process.
	debug.SetGCPercent(-1)
	procs := runtime.GOMAXPROCS(1)
	lim := syscall.Rlimit{Cur: o.MaxMemory, Max: o.MaxMemory}
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, 0, syscall.RLIMIT_AS, uintptr(unsafe.Pointer(&lim)), 0, 0, 0); errno != 0 {
		runtime.GOMAXPROCS(procs)
		debug.SetGCPercent(100)
		return fmt.Errorf("confine: limit the address space to %d bytes: %w", o.MaxMemory, errno)
	}
	_, _, errno := syscall.RawSyscall(syscall.SYS_EXECVE, uintptr(unsafe.Pointer(argv0p)), uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envvp[0])))
	failExec(errno)
	return nil
}

// execFailed prefixes failExec's report.
const execFailed = "confine: exec under the memory limit failed: "

// failExec reports a failed execve on ErrorFD (stderr when the caller
// passed none) and exits ExitFailed, allocating nothing: Errno.Error is a
// constant for every errno execve returns.
func failExec(errno syscall.Errno) {
	msg := errno.Error()
	for _, fd := range []uintptr{ErrorFD, 2} {
		if _, _, e := syscall.RawSyscall(syscall.SYS_WRITE, fd, uintptr(unsafe.Pointer(unsafe.StringData(execFailed))), uintptr(len(execFailed))); e == 0 {
			_, _, _ = syscall.RawSyscall(syscall.SYS_WRITE, fd, uintptr(unsafe.Pointer(unsafe.StringData(msg))), uintptr(len(msg)))
			break
		}
	}
	_, _, _ = syscall.RawSyscall(syscall.SYS_EXIT_GROUP, ExitFailed, 0, 0)
}

// restrict applies o's Landlock ruleset to this thread.
func restrict(o Options) error {
	abi, err := ABI()
	if err != nil {
		return err
	}
	all, file := writeRights(abi)
	reads := len(o.Readable) > 0
	attr := rulesetAttr{handledAccessFS: all}
	if reads {
		attr.handledAccessFS |= readsV1
	}
	size := unsafe.Sizeof(attr.handledAccessFS)
	if o.NoNetwork && NetworkDenied(abi) {
		attr.handledAccessNet = accessNetBindTCP | accessNetConnectTCP
		size = unsafe.Sizeof(attr)
	}
	fd, _, errno := syscall.RawSyscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr)), size, 0)
	if errno != 0 {
		return fmt.Errorf("landlock: create the ruleset: %w", errno)
	}
	defer func() { _ = syscall.Close(int(fd)) }()
	// A ruleset that handles reading must let the program read /dev/null,
	// read what it may write, and run itself.
	var readFile, readTree uint64
	if reads {
		readFile, readTree = accessReadFile, readsV1
	}
	if err := addRule(int(fd), devNull, file|readFile); err != nil {
		return err
	}
	for _, d := range o.Writable {
		if err := addRule(int(fd), d, all|readTree); err != nil {
			return err
		}
	}
	if reads {
		for _, d := range o.Readable {
			if err := addRule(int(fd), d, readsV1); err != nil {
				return err
			}
		}
		if err := addRule(int(fd), o.program, accessExecute|accessReadFile); err != nil {
			return err
		}
	}
	// restrict_self needs no_new_privs, which ExecWith set first.
	if _, _, errno := syscall.RawSyscall(sysLandlockRestrictSelf, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock: restrict: %w", errno)
	}
	return nil
}

// addRule lets the ruleset's process use rights beneath path.
func addRule(ruleset int, path string, rights uint64) error {
	pfd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("landlock: open %s: %w", path, err)
	}
	defer func() { _ = syscall.Close(pfd) }()
	rule := pathBeneathAttr{allowedAccess: rights, parentFd: int32(pfd)} //nolint:gosec // a descriptor fits in an int32
	if _, _, errno := syscall.RawSyscall6(sysLandlockAddRule, uintptr(ruleset), rulePathBeneath, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock: allow access beneath %s: %w", path, errno)
	}
	return nil
}

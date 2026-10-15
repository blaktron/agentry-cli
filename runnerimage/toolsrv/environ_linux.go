//go:build linux

package toolsrv

import "syscall"

// prSetDumpable is PR_SET_DUMPABLE from <linux/prctl.h>; the syscall
// package exports SYS_PRCTL but not the option constants.
const prSetDumpable = 4

// hideEnvironment marks this process non-dumpable.
// The tools server holds every granted secret in its environment, and a
// catalogued exec command runs as the same root in the same pid namespace:
// without this, `cat /proc/<server pid>/environ` hands the model the
// secrets — os.Unsetenv would not help, because /proc/<pid>/environ shows
// the block the process started with. A non-dumpable process's /proc files
// (environ, mem, …) belong to root and pass the kernel's ptrace access check
// only for a caller holding CAP_SYS_PTRACE, which the default docker
// capability set the tool containers run with does not include. This holds
// under both runc and runsc (gVisor). Children are unaffected: execve
// resets the flag for the program it starts.
func hideEnvironment() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

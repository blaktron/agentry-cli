// Package confine holds a declared exec command to the places it may
// write (agentry-cli#596). run_command starts each declared command
// through this binary's `confine` role, which asks the kernel's Landlock
// to refuse every write outside the named directories and then replaces
// itself with the command: the restriction is the kernel's, inherited
// across execve, and nothing the command does can lift it. Reading is not
// restricted. The tools server itself is never confined, so write_report
// and the receipts' ledgers under /out are written as before, while a
// declared command can no longer write there.
//
// Landlock is Linux's (5.13 and later, with the LSM enabled). gVisor does
// not implement it, so ABI reports an error there and the tools server
// falls back to running the command as an unprivileged user
// (runnerimage/toolsrv).
//
// Stdlib only, like the rest of the runner image (RQ1): the syscalls and
// constants of include/uapi/linux/landlock.h are spelled here.
package confine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Role is the runner binary's argument that runs Trampoline.
const Role = "confine"

// ErrorFD is the descriptor the trampoline reports its own failure on.
// The caller passes a pipe's write end there; Exec marks it close-on-exec,
// so a command that started closes it unwritten and never holds it, and
// whatever the caller reads from it is the trampoline's, never the
// command's.
const ErrorFD = 3

// ExitFailed is the trampoline's exit status when it could not confine or
// start the command.
const ExitFailed = 125

// Trampoline is the role's entry point: Main, with its failure written to
// ErrorFD (or to stderr, when the caller passed none) and ExitFailed. It
// does not return.
func Trampoline(args []string) {
	err := Main(args, os.Environ())
	// Only 0, 1, 2 and the caller's pipe are open here (os/exec passes
	// nothing else), so descriptor 3 is the pipe or EBADF.
	if _, werr := os.NewFile(ErrorFD, "confine-error").WriteString(err.Error()); werr != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(ExitFailed)
}

// Main is the trampoline: `confine --write DIR [--write DIR …] -- PROGRAM
// [ARG …]`. It confines this process to writing beneath each DIR and then
// execs PROGRAM with ARGs and this process's environment. It returns only
// on failure, and never runs PROGRAM unconfined.
func Main(args []string, env []string) error {
	var dirs []string
	for len(args) > 0 && args[0] != "--" {
		if args[0] != "--write" || len(args) < 2 {
			return fmt.Errorf("confine: usage: confine --write DIR [--write DIR …] -- PROGRAM [ARG …]")
		}
		if !filepath.IsAbs(args[1]) {
			return fmt.Errorf("confine: --write %q must be an absolute path", args[1])
		}
		dirs = append(dirs, args[1])
		args = args[2:]
	}
	if len(args) < 2 {
		return errors.New("confine: a program after -- is required")
	}
	return Exec(dirs, args[1], args[1:], env)
}

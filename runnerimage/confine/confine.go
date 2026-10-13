// Package confine holds a declared exec command to the places it may
// write (agentry-cli#596). run_command starts each declared command
// through this binary's `confine` role, which asks the kernel's Landlock
// to refuse every write outside the named directories and then replaces
// itself with the command: the restriction is the kernel's, inherited
// across execve, and nothing the command does can lift it. Reading is not
// restricted unless the caller names what may be read (--read: grep and
// glob's ripgrep, plan ripgrep-search.md §5). The tools server itself is
// never confined, so write_report
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
	"strconv"
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

// Options is what the trampoline holds a program to.
type Options struct {
	// Writable are the directories the program may write beneath (with
	// /dev/null), by Landlock.
	Writable []string
	// Readable, when set, are the only directories (or files) the program
	// may read, list and run anything beneath, besides the writable ones, the
	// program itself and /dev/null: Landlock then refuses every other
	// read. Unset, reading is not restricted.
	Readable []string
	// MaxMemory, when set, is the program's address-space limit in bytes
	// (RLIMIT_AS): an allocation past it fails.
	MaxMemory uint64
	// NoNetwork refuses the program every TCP bind and connect where
	// Landlock can (ABI 4 and later); on an older ABI it is not refused,
	// which NetworkDenied tells the caller beforehand.
	NoNetwork bool
	// LimitsOnly applies MaxMemory and no_new_privs and no Landlock: the
	// caller holds the program's writes another way, by starting it as a
	// user that owns nothing it must not write. For a machine without
	// Landlock (gVisor).
	LimitsOnly bool

	// program is the file ExecWith runs, which a ruleset that handles
	// reading must let it execute.
	program string
}

// Main is the trampoline: `confine [--write DIR …] [--read DIR …]
// [--max-memory BYTES] [--no-network] -- PROGRAM [ARG …]`, or `confine
// --limits-only [--max-memory BYTES] -- PROGRAM [ARG …]`. It confines this
// process to writing beneath each --write DIR and, when any --read DIR is
// named, to reading beneath those and the writable ones, with the limits,
// and then execs PROGRAM with
// ARGs and this process's environment. It returns only on failure, and
// never runs PROGRAM with less than it was told.
func Main(args []string, env []string) error {
	const usage = "confine: usage: confine [--write DIR …] [--read DIR …] [--max-memory BYTES] [--no-network] -- PROGRAM [ARG …], or confine --limits-only [--max-memory BYTES] -- PROGRAM [ARG …]"
	var o Options
	for len(args) > 0 && args[0] != "--" {
		switch args[0] {
		case "--write":
			if len(args) < 2 {
				return errors.New(usage)
			}
			if !filepath.IsAbs(args[1]) {
				return fmt.Errorf("confine: --write %q must be an absolute path", args[1])
			}
			o.Writable = append(o.Writable, args[1])
			args = args[2:]
		case "--read":
			if len(args) < 2 {
				return errors.New(usage)
			}
			if !filepath.IsAbs(args[1]) {
				return fmt.Errorf("confine: --read %q must be an absolute path", args[1])
			}
			o.Readable = append(o.Readable, args[1])
			args = args[2:]
		case "--max-memory":
			if len(args) < 2 {
				return errors.New(usage)
			}
			n, err := strconv.ParseUint(args[1], 10, 64)
			if err != nil || n == 0 {
				return fmt.Errorf("confine: --max-memory %q must be a positive number of bytes", args[1])
			}
			o.MaxMemory = n
			args = args[2:]
		case "--no-network":
			o.NoNetwork = true
			args = args[1:]
		case "--limits-only":
			o.LimitsOnly = true
			args = args[1:]
		default:
			return errors.New(usage)
		}
	}
	if o.LimitsOnly && (len(o.Writable) > 0 || len(o.Readable) > 0 || o.NoNetwork) {
		return errors.New("confine: --limits-only applies no Landlock, so it takes no --write, --read or --no-network")
	}
	if len(args) < 2 {
		return errors.New("confine: a program after -- is required")
	}
	return ExecWith(o, args[1], args[1:], env)
}

// Exec confines this process to writing beneath dirs (and to /dev/null),
// then replaces it with path, argv and env. It returns only on failure.
func Exec(dirs []string, path string, argv, env []string) error {
	return ExecWith(Options{Writable: dirs}, path, argv, env)
}

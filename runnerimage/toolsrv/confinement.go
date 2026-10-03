package toolsrv

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/blaktron/agentry-cli/runnerimage/confine"
)

// ConfineRole is the argument that makes this process's own binary the
// confine trampoline: the runner image's `confine`. The CLI's hidden
// runner-tools sets its own command's name.
var ConfineRole = confine.Role

// unprivilegedID is the uid and gid a declared command runs as where
// Landlock is missing: nobody, which owns nothing in the container or in
// the run's mounts.
const unprivilegedID = 65534

// confinement is how run_command holds a declared command to the places it
// may write (agentry-cli#596): the run's output folder is mounted
// read-write into the tools container for write_report and the receipts'
// ledgers, and a declared command runs in the same container.
//
//   - Landlock, where the kernel has it: the command stays root and reads
//     what it read before, and the kernel refuses its every write outside
//     the scratch directory and, when the contract grants writes, the
//     workspace.
//   - Where it does not (gVisor implements no Landlock): the command runs as
//     nobody, which can write only where anyone may, the scratch tmpfs. A
//     contract that grants workspace writes cannot be held that way, so no
//     declared command runs.
//   - Off Linux the tools server is only `agentry runner-tools`, a
//     debugging path that never runs a QuickAgent, and nothing is confined.
type confinement struct {
	// landlock is the ABI version when Landlock holds the writes, 0 when
	// it does not.
	landlock int
	// writable is what Landlock lets a command write beneath.
	writable []string
	// asNobody runs the command as unprivilegedID.
	asNobody bool
	// refused, when set, is why no declared command runs.
	refused string
	// none is the off-Linux debugging path.
	none bool
	// trampoline is the confine role's argv prefix.
	trampoline []string
}

// chooseConfinement picks the confinement for spec on this machine: goos,
// what Landlock answered, this process's euid, and the executable that
// serves the confine role.
func chooseConfinement(spec *Spec, goos string, abi int, abiErr error, euid int, exe string, scratch string) confinement {
	if goos != "linux" {
		return confinement{none: true}
	}
	writable := []string{scratch}
	if spec.FSWrite.Enabled {
		writable = append(writable, spec.WorkspaceDir)
	}
	switch {
	case abiErr == nil:
		return confinement{landlock: abi, writable: writable, trampoline: []string{exe, ConfineRole}}
	case spec.FSWrite.Enabled:
		return confinement{refused: fmt.Sprintf("this machine cannot hold a command to the paths it may write (%v), and this QuickAgent may write its workspace, so no declared command runs", abiErr)}
	case euid != 0:
		return confinement{refused: fmt.Sprintf("this machine cannot hold a command to the paths it may write (%v), and the tools server cannot start one as another user", abiErr)}
	default:
		return confinement{asNobody: true}
	}
}

// newConfinement is chooseConfinement for this process.
func newConfinement(spec *Spec) confinement {
	abi, abiErr := confine.ABI()
	exe, err := os.Executable()
	if err != nil {
		exe = ""
		if abiErr == nil {
			abiErr = fmt.Errorf("this binary cannot find itself: %w", err)
		}
	}
	return chooseConfinement(spec, runtime.GOOS, abi, abiErr, os.Geteuid(), exe, os.TempDir())
}

// argv is the program and arguments that start the declared command at
// path with args: the trampoline under Landlock, the command itself
// otherwise (asNobody is applied by runAs).
func (c confinement) argv(path string, args []string) (string, []string) {
	if c.landlock == 0 {
		return path, args
	}
	argv := append([]string{}, c.trampoline[1:]...)
	for _, d := range c.writable {
		argv = append(argv, "--write", d)
	}
	return c.trampoline[0], append(append(argv, "--", path), args...)
}

// run starts cmd and waits for it. Under Landlock the trampoline that could
// not confine or start the command says why on confine.ErrorFD, and that
// comes back as failed: the command did not run.
func (c confinement) run(cmd *exec.Cmd) (failed string, err error) {
	if c.landlock == 0 {
		return "", cmd.Run()
	}
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	cmd.ExtraFiles = []*os.File{w} // the first extra file is descriptor 3, confine.ErrorFD
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		return "", err
	}
	err = cmd.Wait()
	msg, _ := io.ReadAll(io.LimitReader(r, 4096))
	return strings.TrimSpace(string(msg)), err
}

// describe is the sentence run_command's description and the server's
// log give the confinement.
func (c confinement) describe() string {
	switch {
	case c.refused != "":
		return "No declared command runs: " + c.refused + "."
	case c.landlock > 0:
		return fmt.Sprintf("It can write only beneath %s (Landlock ABI %d).", strings.Join(c.writable, " and "), c.landlock)
	case c.asNobody:
		return "It runs as an unprivileged user, so it can write only where anyone may, such as /tmp (this machine has no Landlock)."
	default:
		return "Its writes are not confined (a debugging server off Linux)."
	}
}

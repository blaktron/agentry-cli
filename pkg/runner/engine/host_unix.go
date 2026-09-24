//go:build !windows

package engine

import (
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/owner"
)

// procStartTime is owner.StartTime: the pid's start time, which tells the
// group this run started from a group a reused pid now leads.
func procStartTime(pid int) (uint64, bool) { return owner.StartTime(pid) }

// configProcess puts the harness in its own process group so a budget or
// timeout kill reaches every child it spawned, gives it SIGTERM first and
// SIGKILL after grace.
//
// The group is SIGKILLed here because exec's own escalation when WaitDelay
// expires is os.Process.Kill on the leader's single pid, so a child that
// ignored SIGTERM survived the run holding the model key (agentry-cli#161).
//
// Signalling a group id is only safe while the id still belongs to us, and
// Setpgid makes the leader's pid the group's id. cmd.Wait reaps the leader
// before it waits on the I/O goroutines, so that pid is free while this
// escalation is still pending — and the pending case is exactly the one it
// exists for, a child holding stdout open. Two guards cover it. procStartTime
// refuses the kill when the pid now belongs to a different process: a reused
// pid is necessarily live and so has a /proc record, while a group that is
// still ours has no record once its leader has been reaped, and kill(-pgid)
// can then only reach our own child. And the returned function calls a pending
// escalation off once the process has been waited on, which covers the prompt
// exit where there is nothing left to escalate to. Off Linux there is no start
// time to compare and the pid alone is all there is.
//
// grace is a parameter rather than a package variable so a test can shorten it
// without a write that the escalation goroutine would race on.
func configProcess(cmd *exec.Cmd, grace time.Duration) func() {
	waited := make(chan struct{})
	var once sync.Once
	done := func() { once.Do(func() { close(waited) }) }

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pgid := cmd.Process.Pid
		// Read before the SIGTERM: the leader is alive at this point, so this
		// is its own start time and not a reused pid's.
		start, _ := procStartTime(pgid)
		if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
			return err
		}
		go func() {
			select {
			case <-time.After(grace):
				if now, ok := procStartTime(pgid); ok && now != start {
					return // the kernel has given the pid to something else
				}
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			case <-waited:
			}
		}()
		return nil
	}
	// Past our own escalation, so exec's single-pid kill is the backstop
	// rather than the first resort.
	cmd.WaitDelay = grace + 2*time.Second
	return done
}

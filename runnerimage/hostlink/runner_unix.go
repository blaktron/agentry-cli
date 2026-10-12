//go:build unix

package hostlink

import (
	"context"
	"io"
	"os/exec"
	"syscall"
)

// execRunner runs argv in its own process group, killed whole when ctx
// ends, so a timed-out ssh leaves nothing behind.
func execRunner(ctx context.Context, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // ssh or sftp, with arguments sshOptions builds
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return exitCode(cmd, err)
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return -1, ctx.Err()
	}
}

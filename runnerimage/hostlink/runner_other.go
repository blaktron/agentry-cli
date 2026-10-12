//go:build !unix

package hostlink

import (
	"context"
	"io"
	"os/exec"
)

// execRunner runs argv to its end, killed when ctx ends. The host link runs
// in a Linux container; this build exists so the CLI that names its
// constants builds everywhere.
func execRunner(ctx context.Context, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // ssh or sftp, with arguments sshOptions builds
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	return exitCode(cmd, err)
}

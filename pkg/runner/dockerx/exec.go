package dockerx

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// Execer runs a command in a running container with its stdin and stdout
// streamed, the way `docker exec -i` does: an optional capability of a
// Client, asked for with a type assertion so a Client that lacks it (a
// test's own, another program's) still satisfies Client. The CLI client
// has it, and so does Fake.
type Execer interface {
	// ExecStream runs argv in container as the container's user and
	// returns its exit code. A non-zero exit is not an error; err means
	// docker itself failed. stderr is discarded.
	ExecStream(ctx context.Context, container string, argv []string, stdin io.Reader, stdout io.Writer) (int, error)
}

// ExecStream implements Execer.
func (c *CLI) ExecStream(ctx context.Context, container string, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
	cmd := c.command(ctx, append([]string{"exec", "-i", container}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, io.Discard
	// A stdin that is not a file (a relayed connection) is copied by a
	// goroutine Wait would otherwise wait on until the other side closes.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if asExitError(err, &exitErr) {
		code := exitErr.ExitCode()
		if code >= 125 && ctx.Err() == nil {
			return code, fmt.Errorf("docker exec in %s failed (exit %d)", container, code)
		}
		return code, nil
	}
	return -1, fmt.Errorf("docker exec in %s: %w", container, err)
}

// ExecStream implements Execer: Exec answers it, recorded as
// "ExecStream:<container>"; with no Exec it reads stdin to its end and
// exits 0.
func (f *Fake) ExecStream(ctx context.Context, container string, argv []string, stdin io.Reader, stdout io.Writer) (int, error) {
	f.record("ExecStream", container, RunSpec{Name: container, Argv: argv})
	if f.Exec != nil {
		return f.Exec(ctx, container, argv, stdin, stdout)
	}
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	return 0, nil
}

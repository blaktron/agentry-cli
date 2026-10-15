//go:build unix

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// openProtocolEvents is descriptor 3, where the session protocol writes its
// events. It must be a pipe or a socket the driver opened for writing.
// Anything else means the driver opened nothing there: the Go runtime fills
// the lowest free descriptor with its own files at start (the cgroup's
// cpu.max, the netpoller's epoll), which a bare Stat accepts, and events
// written to them would be lost without a word.
func openProtocolEvents() (io.Writer, error) {
	const fd = 3
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("--session-protocol writes its events to descriptor 3, and it is not open: %w", err)
	}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFIFO, unix.S_IFSOCK:
	default:
		return nil, errors.New("--session-protocol writes its events to descriptor 3, and it is not a pipe or a socket the driver opened")
	}
	fl, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
	if err != nil {
		return nil, fmt.Errorf("--session-protocol: descriptor 3: %w", err)
	}
	if fl&unix.O_ACCMODE == unix.O_RDONLY {
		return nil, errors.New("--session-protocol writes its events to descriptor 3, and it is open for reading only")
	}
	// The descriptor came from the driver without close-on-exec, and a
	// descriptor without it survives every exec: every process the CLI
	// starts (the docker client, a host-run harness) would inherit the
	// events, could write lines the driver takes as the session's, and would
	// hold the pipe open past the CLI.
	unix.CloseOnExec(fd)
	return os.NewFile(fd, "session-events"), nil
}

// ignoreBrokenPipes keeps a driver that closes stdout or stderr from killing
// the process mid-session: Go dies of SIGPIPE on a write to a closed fd 1 or
// 2, and a session killed so skips its teardown. Ignored, the write fails
// and the session ends and tears down on its own terms.
func ignoreBrokenPipes() { signal.Ignore(syscall.SIGPIPE) }

//go:build windows

package main

import (
	"errors"
	"io"
)

// openProtocolEvents: Windows has no descriptor 3 to inherit, and the
// session protocol's driver (a sandbox host) runs on Linux.
func openProtocolEvents() (io.Writer, error) {
	return nil, errors.New("--session-protocol is not available on Windows: it writes its events to descriptor 3")
}

func ignoreBrokenPipes() {}

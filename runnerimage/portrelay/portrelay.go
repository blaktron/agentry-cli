// Package portrelay carries one TCP connection to a port of the container's
// own loopback over stdio: the runner execs it in the Coding agent's tools
// server for each connection to a port published on the person's computer.
// It reaches the container's own network namespace only, so it works however
// the container's network is set, and it imports only the standard library:
// the runner image compiles it from its embedded sources.
package portrelay

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Role is the runner binary's mode that runs Pipe.
const Role = "port-relay"

// dialTimeout bounds the connect to the program serving the port.
const dialTimeout = 5 * time.Second

// ParsePort reads a published port's number: 1024 to 65535, the ports a
// program in the container serves without privilege.
func ParsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1024 || p > 65535 {
		return 0, fmt.Errorf("%q is not a port from 1024 to 65535", s)
	}
	return p, nil
}

// Pipe connects to 127.0.0.1:port and copies in to the connection and the
// connection to out until either side ends. A port nothing listens on is an
// error, said on the caller's error stream; the person's browser then sees
// the connection close.
func Pipe(port int, in io.Reader, out io.Writer) error {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), dialTimeout)
	if err != nil {
		return fmt.Errorf("nothing answers on port %d in the container: %w", port, err)
	}
	defer func() { _ = c.Close() }()
	tc, _ := c.(*net.TCPConn)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(c, in)
		// The person's side finished sending: tell the program, and keep
		// reading its answer.
		if tc != nil {
			_ = tc.CloseWrite()
		}
	}()
	_, err = io.Copy(out, c)
	_ = c.Close()
	wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

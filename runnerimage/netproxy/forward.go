package netproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Forward accepts TCP connections on l and pipes each to target — a plain
// byte pipe, no inspection. It exists so a host-run harness can reach the
// gateway: the gateway stays on the run's internal network (a container
// that joins the bridge as well makes every MCP server it launches inherit
// that mix of networks, which docker refuses), and this listener on the
// proxy container — the one run container with a bridge leg — is the
// host's door to it. Returns when ctx ends or the listener closes.
func Forward(ctx context.Context, l net.Listener, target string, logw io.Writer) error {
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go pipe(ctx, c, target, logw)
	}
}

func pipe(ctx context.Context, c net.Conn, target string, logw io.Writer) {
	defer func() { _ = c.Close() }()
	d := net.Dialer{Timeout: 10 * time.Second}
	up, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		if logw != nil {
			_, _ = fmt.Fprintf(logw, "forward: dial %s: %v\n", target, err)
		}
		return
	}
	defer func() { _ = up.Close() }()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(time.Second):
	}
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// ParseForward splits a "LISTEN=TARGET" flag value ("":8811=gateway:8811").
func ParseForward(v string) (listen, target string, err error) {
	parts := strings.Split(v, "=")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("--forward wants LISTEN=TARGET, got %q", v)
	}
	return parts[0], parts[1], nil
}

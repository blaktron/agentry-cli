package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/portrelay"
	"github.com/blaktron/agentry-cli/runnerimage/sockfilter"
)

// The Coding agent's published ports. For a segment whose roster has a port
// on, this process listens on this computer's loopback at the port's number
// and carries each connection into the tools server's own loopback at the
// same number: the runner binary's port-relay mode, run through the engine's
// exec, pipes it. The engine publishes no port of any container, so the
// relay reaches whatever the tools server's network is (none included), and
// only the ports the roster names are listened on. The gateway starts the
// tools server when the agent first calls a tool, so the relay listens from
// the segment's start and finds the container, by the label the docker
// filter gives it alone, when a connection comes; it ends with the segment.

// PortRelayLog is the receipt of what the relay published and refused.
const PortRelayLog = "port-relay.log"

// runnerBinary is the runner binary in every image built on the runner.
const runnerBinary = "/usr/local/bin/agentry-runner"

// maxRelayedConns bounds the connections relayed at once, all ports
// together; one past it is closed at once.
const maxRelayedConns = 64

// portListen listens on a loopback address; a variable so a test can
// refuse one.
var portListen = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

// startPortRelays publishes the segment's ports. A port another program on
// this computer holds publishes nothing, and an engine client that cannot
// exec publishes no port: the receipt says so, and the segment runs without.
func startPortRelays(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID string) error {
	c := p.Plan.Capabilities.Coder
	if p.Persistent == nil || c == nil || !c.Commands || len(c.Ports) == 0 {
		return nil
	}
	logf, err := os.OpenFile(filepath.Join(rd.receiptsDir, PortRelayLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // a receipt; no secret in it
	if err != nil {
		return Classify(ExitCLI, err)
	}
	var logMu sync.Mutex
	logLine := func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		_, _ = fmt.Fprintf(logf, time.Now().UTC().Format(time.RFC3339)+" "+format+"\n", a...)
	}
	ex, ok := d.(dockerx.Execer)
	if !ok {
		logLine("no port is published: this engine client cannot exec in a container")
		_ = logf.Close()
		return nil
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &portRelay{ex: ex, d: d, label: sockfilter.ToolsLabel + "=" + runID, log: logLine, slots: make(chan struct{}, maxRelayedConns)}
	for _, port := range c.Ports {
		r.publish(rctx, port)
	}
	td.add(func(context.Context) {
		cancel()
		r.close()
		_ = logf.Close()
	})
	return nil
}

// portRelay is a segment's listeners and the connections they carry.
type portRelay struct {
	ex    dockerx.Execer
	d     dockerx.Client
	label string
	log   func(format string, a ...any)
	slots chan struct{}
	mu    sync.Mutex
	// container is the tools server, once found; missing says the last
	// connection found none, so the receipt says it once until one is.
	container string
	missing   bool
	listeners []net.Listener
	wg        sync.WaitGroup
}

// toolsServer is the container carrying the tools server's label: the one
// found before, or the one the engine lists now.
func (r *portRelay) toolsServer(ctx context.Context) (string, error) {
	r.mu.Lock()
	name := r.container
	r.mu.Unlock()
	if name != "" {
		return name, nil
	}
	names, err := r.d.PSByLabel(ctx, r.label)
	if err != nil {
		return "", fmt.Errorf("list the tools server: %w", err)
	}
	switch len(names) {
	case 0:
		return "", errNoToolsServer
	case 1:
	default:
		return "", fmt.Errorf("%d containers carry the tools server's label, where one should", len(names))
	}
	r.mu.Lock()
	r.container, r.missing = names[0], false
	r.mu.Unlock()
	r.log("the tools server is %s", names[0])
	return names[0], nil
}

// forget drops the container found, after an exec in it failed: the
// gateway may have started the tools server again.
func (r *portRelay) forget(name string) {
	r.mu.Lock()
	if r.container == name {
		r.container = ""
	}
	r.mu.Unlock()
}

// errNoToolsServer is a connection that came before the tools server runs.
var errNoToolsServer = errors.New("the tools server is not running yet (it starts at the agent's first tool call)")

// publish listens at the port on 127.0.0.1, and on ::1 where this computer
// has it (a browser may resolve localhost to either).
func (r *portRelay) publish(ctx context.Context, port spec.CoderPort) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port.Port))
	l4, err := portListen(addr)
	if err != nil {
		r.log("port %s (%d) is not published: %v", port.Name, port.Port, err)
		return
	}
	ls := []net.Listener{l4}
	if l6, err := portListen(net.JoinHostPort("::1", strconv.Itoa(port.Port))); err == nil {
		ls = append(ls, l6)
	}
	r.mu.Lock()
	r.listeners = append(r.listeners, ls...)
	r.mu.Unlock()
	r.log("port %s published at http://%s, carried into the tools server's own 127.0.0.1:%d", port.Name, addr, port.Port)
	for _, l := range ls {
		r.wg.Add(1)
		go r.accept(ctx, l, port)
	}
}

func (r *portRelay) accept(ctx context.Context, l net.Listener, port spec.CoderPort) {
	defer r.wg.Done()
	argv := []string{runnerBinary, portrelay.Role, strconv.Itoa(port.Port)}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		select {
		case r.slots <- struct{}{}:
		default:
			r.log("port %s: a connection past %d at once was closed", port.Name, maxRelayedConns)
			_ = c.Close()
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() { <-r.slots }()
			done := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
				case <-done:
				}
				_ = c.Close()
			}()
			defer close(done)
			container, err := r.toolsServer(ctx)
			if err != nil {
				r.mu.Lock()
				say := !r.missing || !errors.Is(err, errNoToolsServer)
				r.missing = errors.Is(err, errNoToolsServer)
				r.mu.Unlock()
				if say {
					r.log("port %s: a connection was closed: %v", port.Name, err)
				}
				return
			}
			if _, err := r.ex.ExecStream(ctx, container, argv, c, c); err != nil && ctx.Err() == nil {
				r.forget(container)
				r.log("port %s: %v", port.Name, err)
			}
		}()
	}
}

// close stops listening, ends every connection and waits for them.
func (r *portRelay) close() {
	r.mu.Lock()
	for _, l := range r.listeners {
		_ = l.Close()
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

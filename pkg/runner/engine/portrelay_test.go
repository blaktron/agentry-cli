package engine

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/portrelay"
)

// freePort is a loopback port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func portsParams(ports ...spec.CoderPort) Params {
	return Params{
		Plan:       &spec.Plan{Capabilities: spec.CapSummary{Coder: &spec.CoderGrant{Commands: true, Ports: ports}}},
		Persistent: &PersistentSegment{},
	}
}

// TestAPublishedPortIsCarriedIntoTheToolsServer: a connection to a port the
// roster names reaches the tools server's own port through the runner's
// port-relay mode, a port another program holds is not published and the
// receipt says so, and the segment's teardown stops listening.
func TestAPublishedPortIsCarriedIntoTheToolsServer(t *testing.T) {
	web := freePort(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	heldPort := held.Addr().(*net.TCPAddr).Port

	var argvs [][]string
	f := &dockerx.Fake{ByLabel: map[string][]string{"agentry.tools=run1": {"agentry-tools-1"}}}
	f.Exec = func(_ context.Context, container string, argv []string, in io.Reader, out io.Writer) (int, error) {
		argvs = append(argvs, append([]string{container}, argv...))
		line, _ := bufio.NewReader(in).ReadString('\n')
		_, _ = io.WriteString(out, "echo: "+line)
		return 0, nil
	}
	rd := &runDirs{receiptsDir: t.TempDir()}
	td := &teardown{}
	p := portsParams(spec.CoderPort{Name: "web", Port: web}, spec.CoderPort{Name: "busy", Port: heldPort})
	if err := startPortRelays(context.Background(), f, p, rd, td, "run1"); err != nil {
		t.Fatal(err)
	}

	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(web)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "GET /\n")
	got, _ := io.ReadAll(c)
	_ = c.Close()
	if string(got) != "echo: GET /\n" {
		t.Errorf("answer = %q", got)
	}
	want := []string{"agentry-tools-1", runnerBinary, portrelay.Role, strconv.Itoa(web)}
	if len(argvs) != 1 || !slices.Equal(argvs[0], want) {
		t.Errorf("exec = %q, want %q", argvs, want)
	}
	for _, fn := range slices.Backward(td.funcs) {
		fn(context.Background())
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(web)), time.Second); err == nil {
		_ = c.Close()
		t.Error("the port still answers after the segment's teardown")
	}
	log, _ := os.ReadFile(filepath.Join(rd.receiptsDir, PortRelayLog))
	if !strings.Contains(string(log), "port web published at http://127.0.0.1:"+strconv.Itoa(web)) ||
		!strings.Contains(string(log), "port busy ("+strconv.Itoa(heldPort)+") is not published") {
		t.Errorf("receipt:\n%s", log)
	}
}

// startingTools lists the tools server once started is set, as the engine
// does once the gateway has started it.
type startingTools struct {
	*dockerx.Fake
	started atomic.Bool
}

func (s *startingTools) PSByLabel(_ context.Context, label string) ([]string, error) {
	if s.started.Load() && label == "agentry.tools=run1" {
		return []string{"agentry-tools-1"}, nil
	}
	return nil, nil
}

// TestAConnectionBeforeTheToolsServerIsClosed: the port is listened on from
// the segment's start; a connection before the gateway has started the tools
// server is closed, which the receipt says once, and one after it reaches
// the container; a segment with no port, or whose programs are off, does
// nothing at all.
func TestAConnectionBeforeTheToolsServerIsClosed(t *testing.T) {
	port := freePort(t)
	f := &startingTools{Fake: &dockerx.Fake{}}
	f.Exec = func(_ context.Context, _ string, _ []string, _ io.Reader, out io.Writer) (int, error) {
		_, _ = io.WriteString(out, "served")
		return 0, nil
	}
	rd := &runDirs{receiptsDir: t.TempDir()}
	td := &teardown{}
	if err := startPortRelays(context.Background(), f, portsParams(spec.CoderPort{Name: "web", Port: port}), rd, td, "run1"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, fn := range slices.Backward(td.funcs) {
			fn(context.Background())
		}
	}()
	get := func() string {
		c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(c)
		return string(b)
	}
	for range 2 {
		if got := get(); got != "" {
			t.Fatalf("before the tools server: %q", got)
		}
	}
	f.started.Store(true)
	if got := get(); got != "served" {
		t.Fatalf("after the tools server started: %q", got)
	}
	log, _ := os.ReadFile(filepath.Join(rd.receiptsDir, PortRelayLog))
	if n := strings.Count(string(log), "not running yet"); n != 1 || !strings.Contains(string(log), "the tools server is agentry-tools-1") {
		t.Errorf("receipt (the missing server said %d times):\n%s", n, log)
	}

	off := portsParams(spec.CoderPort{Name: "web", Port: port})
	off.Plan.Capabilities.Coder.Commands = false
	noSegment := portsParams(spec.CoderPort{Name: "web", Port: port})
	noSegment.Persistent = nil
	for name, p := range map[string]Params{"no ports": portsParams(), "programs off": off, "no persistent segment": noSegment} {
		rd := &runDirs{receiptsDir: t.TempDir()}
		if err := startPortRelays(context.Background(), &dockerx.Fake{}, p, rd, &teardown{}, "run1"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(rd.receiptsDir, PortRelayLog)); err == nil {
			t.Errorf("%s: a port relay receipt was written", name)
		}
	}
}

// The relay listens on the loopback only, on both families, carries at most
// maxRelayedConns connections at once and closes the one past the bound, and
// ends the connections it carries with the segment.
func TestThePortRelayListensOnLoopbackAndBoundsItsConnections(t *testing.T) {
	port := freePort(t)
	var listened []string
	was := portListen
	portListen = func(addr string) (net.Listener, error) { listened = append(listened, addr); return was(addr) }
	t.Cleanup(func() { portListen = was })

	f := &dockerx.Fake{ByLabel: map[string][]string{"agentry.tools=run1": {"agentry-tools-1"}}}
	started := make(chan struct{}, 2*maxRelayedConns)
	f.Exec = func(ctx context.Context, _ string, _ []string, _ io.Reader, _ io.Writer) (int, error) {
		started <- struct{}{}
		<-ctx.Done()
		return 0, ctx.Err()
	}
	rd := &runDirs{receiptsDir: t.TempDir()}
	td := &teardown{}
	if err := startPortRelays(context.Background(), f, portsParams(spec.CoderPort{Name: "web", Port: port}), rd, td, "run1"); err != nil {
		t.Fatal(err)
	}
	torn := false
	teardownRelay := func() {
		if !torn {
			torn = true
			for _, fn := range slices.Backward(td.funcs) {
				fn(context.Background())
			}
		}
	}
	defer teardownRelay()
	if len(listened) == 0 || listened[0] != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) ||
		(len(listened) == 2 && listened[1] != net.JoinHostPort("::1", strconv.Itoa(port))) || len(listened) > 2 {
		t.Errorf("listened on %v, want the IPv4 loopback and, where there is one, the IPv6 loopback", listened)
	}

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var held []net.Conn
	for range maxRelayedConns {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		<-started // carried: its exec began
	}
	past, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = past.SetDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(past); err != nil || len(b) != 0 {
		t.Errorf("the connection past the bound was not closed at once: %q %v", b, err)
	}
	_ = past.Close()
	teardownRelay()
	for i, c := range held {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadAll(c); err != nil {
			t.Fatalf("connection %d outlived the segment: %v", i, err)
		}
		_ = c.Close()
	}
	log, _ := os.ReadFile(filepath.Join(rd.receiptsDir, PortRelayLog))
	if !strings.Contains(string(log), "port web: a connection past "+strconv.Itoa(maxRelayedConns)+" at once was closed") {
		t.Errorf("receipt:\n%s", log)
	}

	// Two containers carrying the label are no one tools server: nothing is
	// carried and the receipt says how many there are.
	f2 := &dockerx.Fake{ByLabel: map[string][]string{"agentry.tools=run1": {"agentry-tools-1", "agentry-tools-2"}}}
	f2.Exec = func(context.Context, string, []string, io.Reader, io.Writer) (int, error) {
		t.Error("a connection was carried into one of two tools servers")
		return 0, nil
	}
	rd2, td2 := &runDirs{receiptsDir: t.TempDir()}, &teardown{}
	port2 := freePort(t)
	if err := startPortRelays(context.Background(), f2, portsParams(spec.CoderPort{Name: "web", Port: port2}), rd2, td2, "run1"); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port2)))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.ReadAll(c)
	_ = c.Close()
	for _, fn := range slices.Backward(td2.funcs) {
		fn(context.Background())
	}
	if log, _ := os.ReadFile(filepath.Join(rd2.receiptsDir, PortRelayLog)); !strings.Contains(string(log), "2 containers carry the tools server's label") {
		t.Errorf("receipt:\n%s", log)
	}
}

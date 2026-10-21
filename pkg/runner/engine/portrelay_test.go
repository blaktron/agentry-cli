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
	for name, p := range map[string]Params{"no ports": portsParams(), "programs off": off} {
		rd := &runDirs{receiptsDir: t.TempDir()}
		if err := startPortRelays(context.Background(), &dockerx.Fake{}, p, rd, &teardown{}, "run1"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(rd.receiptsDir, PortRelayLog)); err == nil {
			t.Errorf("%s: a port relay receipt was written", name)
		}
	}
}

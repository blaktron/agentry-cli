package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// shortDir is a temporary folder whose sockets fit a socket's name.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeSupervisor serves an app link socket that answers each call with its
// tool's name, recording every line it was sent.
type fakeSupervisor struct {
	mu    sync.Mutex
	lines []string
}

func (f *fakeSupervisor) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lines)
}

func serveFakeSupervisor(t *testing.T, sock string) *fakeSupervisor {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeSupervisor{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				sc := bufio.NewScanner(c)
				if !sc.Scan() {
					return
				}
				f.mu.Lock()
				f.lines = append(f.lines, sc.Text())
				f.mu.Unlock()
				req, err := applink.ReadRequest(strings.NewReader(sc.Text() + "\n"))
				if err != nil {
					return
				}
				b, _ := json.Marshal(applink.Answer{Text: "answered " + req.Tool})
				_, _ = c.Write(append(b, '\n'))
			}()
		}
	}()
	return f
}

// relayRig is a segment's run whose daemon runs in a VM: its app link
// socket is the stand-in supervisor's, and the fake engine plays the relay
// container with the real relay, its socket at relaySock.
func relayRig(t *testing.T) (d *dockerx.Fake, rd *runDirs, td *teardown, sup *fakeSupervisor, relaySock string) {
	t.Helper()
	dir := shortDir(t)
	rd = &runDirs{root: filepath.Join(dir, "R"), receiptsDir: filepath.Join(dir, "rc")}
	if err := os.MkdirAll(rd.receiptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rd.appLink = filepath.Join(dir, "sup.sock")
	rd.appLinkRelay, rd.appLinkWhy = appLinkVolume("R"), "the daemon runs in a VM"
	sup = serveFakeSupervisor(t, rd.appLink)
	relaySock = filepath.Join(dir, "relay.sock")
	d = &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-applink-relay": {Handler: func(ctx context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			err := applink.Relay(ctx, applink.RelayConfig{Listen: relaySock, In: stdin, Out: stdout, Log: stderr})
			if err != nil {
				return 1, err
			}
			return 0, nil
		}},
	}}
	td = &teardown{d: d}
	return d, rd, td, sup, relaySock
}

// Where the daemon runs in a VM, the app link's calls reach the supervisor
// through a relay of the run's own: a container on no network, as root with
// every capability dropped but CHOWN and a read-only root, holding the run's
// relay volume alone, attached to this process by its stdio. A call made on
// the relay's socket is answered by the supervisor's socket; its teardown
// removes the container before the volume.
func TestTheAppLinkRelayCarriesCallsToTheSupervisor(t *testing.T) {
	d, rd, td, sup, relaySock := relayRig(t)
	p := Params{Plan: &spec.Plan{Proxy: spec.ProxySpec{Image: "agentry-runner:test"}}}
	if err := startAppLinkRelay(context.Background(), d, p, rd, td, "R"); err != nil {
		t.Fatal(err)
	}
	var rs dockerx.RunSpec
	for _, c := range d.Calls {
		if c.Op == "RunForeground" && c.Arg == containerName("R", "applink-relay") {
			rs = c.Run
		}
	}
	if rs.Name == "" {
		t.Fatalf("no relay container: %+v", d.Calls)
	}
	if rs.Network != "none" || rs.User != "0" || !rs.ReadOnly || !rs.Interactive || rs.Image != "agentry-runner:test" ||
		!slices.Equal(rs.CapDrop, []string{"ALL"}) || !slices.Equal(rs.CapAdd, []string{"CHOWN"}) {
		t.Errorf("the relay's posture: %+v", rs)
	}
	if len(rs.Mounts) != 1 || rs.Mounts[0] != (dockerx.Mount{Source: appLinkVolume("R"), Target: applink.SocketDir}) {
		t.Errorf("the relay's mounts: %+v", rs.Mounts)
	}
	if len(rs.Argv) != 3 || rs.Argv[0] != "applink-relay" || rs.Argv[1] != "--owner" || !strings.Contains(rs.Argv[2], ":") {
		t.Errorf("the relay's argv: %v", rs.Argv)
	}
	a, err := applink.Call(context.Background(), relaySock, applink.Request{Tool: applink.SelfConfigure, Arguments: json.RawMessage(`{"name":"Sandra"}`)})
	if err != nil || a.Text != "answered "+applink.SelfConfigure {
		t.Fatalf("answer %+v, %v", a, err)
	}
	if got := sup.got(); len(got) != 1 || got[0] != `{"tool":"self_configure","arguments":{"name":"Sandra"}}` {
		t.Fatalf("the supervisor was sent %q", got)
	}
	d.Calls = nil
	td.run()
	var order []string
	for _, c := range d.Calls {
		if c.Op == "RemoveForce" || c.Op == "VolumeRemove" {
			order = append(order, c.Op+" "+c.Arg)
		}
	}
	if !slices.Equal(order, []string{"RemoveForce " + containerName("R", "applink-relay"), "VolumeRemove " + appLinkVolume("R")}) {
		t.Errorf("teardown order: %v", order)
	}
}

// Nothing but a tool call reaches the supervisor through the relay: the
// owner socket's commands are refused at the relay's socket, and a frame
// from the relay's side that is not a call is dropped by the bridge.
func TestTheAppLinkRelayCarriesNothingButToolCalls(t *testing.T) {
	d, rd, td, sup, relaySock := relayRig(t)
	defer td.run()
	p := Params{Plan: &spec.Plan{Proxy: spec.ProxySpec{Image: "agentry-runner:test"}}}
	if err := startAppLinkRelay(context.Background(), d, p, rd, td, "R"); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{`{"cmd":"change","entry":"share","state":"on"}`, `{"cmd":"stop"}`, `{"cmd":"send","text":"hi"}`} {
		c, err := net.Dial("unix", relaySock)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte(line + "\n"))
		sc := bufio.NewScanner(c)
		if !sc.Scan() || !strings.Contains(sc.Text(), "isError") {
			t.Errorf("%s: answered %q", line, sc.Text())
		}
		_ = c.Close()
	}
	if got := sup.got(); len(got) != 0 {
		t.Fatalf("the supervisor was sent %q", got)
	}

	// The bridge alone: a relay that sends a frame that is not a call.
	sup2Sock := filepath.Join(shortDir(t), "s.sock")
	sup2 := serveFakeSupervisor(t, sup2Sock)
	inR, inW := io.Pipe()
	var answers syncBuffer
	b := &appLinkBridge{socket: sup2Sock, out: &answers, log: io.Discard}
	done := make(chan error, 1)
	go func() { done <- b.start(context.Background(), inR, 5*time.Second) }()
	for _, line := range []string{
		`{"ready":true}`,
		`{"id":1,"answer":{"text":"x"}}`,
		`{"id":2,"request":{"tool":"self_configure"},"cmd":"change"}`,
		`{"id":3,"request":{"tool":""}}`,
		`{"request":{"tool":"self_configure"}}`,
		`{"id":4,"request":{"tool":"self_configure","arguments":{}}}`,
	} {
		if _, err := inW.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(answers.String(), `"id":4`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = inW.Close()
	if got := sup2.got(); len(got) != 1 || got[0] != `{"tool":"self_configure","arguments":{}}` {
		t.Fatalf("the supervisor was sent %q", got)
	}
	if !strings.Contains(answers.String(), `{"id":4,"answer":{"text":"answered self_configure"}}`) {
		t.Fatalf("the relay was answered %q", answers.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The app link's one mount and its root follow the transport: the
// supervisor's socket at the socket's path, a root of its own, where the
// daemon runs here; the relay's volume at the socket's folder, and no root,
// where it runs in a VM, whose empty entry the gateway's lists leave out.
func TestTheAppLinksMountFollowsTheTransport(t *testing.T) {
	sock := "/home/me/.agentry/runner/s/agent-assistant-app.sock"
	here := &runDirs{appLink: sock}
	if got := here.replacer().Replace(spec.PHAppLinkMount); got != sock+":"+applink.SocketMount+":rw" {
		t.Errorf("here, the mount is %q", got)
	}
	if got := here.replacer().Replace(spec.PHAppLink); got != sock {
		t.Errorf("here, the root is %q", got)
	}
	vm := &runDirs{appLink: sock, appLinkRelay: appLinkVolume("R")}
	if got := vm.replacer().Replace(spec.PHAppLinkMount); got != appLinkVolume("R")+":"+applink.SocketDir+":rw" {
		t.Errorf("in a VM, the mount is %q", got)
	}
	if got := vm.replacer().Replace(spec.PHAppLink); got != "" {
		t.Errorf("in a VM, the root is %q", got)
	}
	plan := &spec.Plan{Gateway: spec.GatewaySpec{Env: []spec.EnvVar{
		{Name: "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS", Value: spec.PHOut + ":" + spec.PHAppLink},
		{Name: "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS", Value: spec.PHAppLink + ":" + spec.PHOut},
	}}}
	vm.outDir = "/home/me/agentry-out/x"
	env, err := vm.gatewayEnv(plan, "token")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS", "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"} {
		if env[k] != vm.outDir {
			t.Errorf("%s = %q", k, env[k])
		}
	}
}

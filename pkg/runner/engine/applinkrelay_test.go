package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
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
	rd.appLinkRelay, rd.appLinkPlacement = appLinkVolume("R"), "the daemon runs in a VM"
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
	lim := p.limits()
	if lim.Memory == "" || rs.Memory != lim.Memory || rs.CPUs != lim.CPUs || rs.PidsLimit != lim.Pids || !rs.NoNewPrivileges {
		t.Errorf("the relay is not bounded as the run's containers are: %+v, limits %+v", rs, lim)
	}
	if rs.Labels[runLabel] != "R" {
		t.Errorf("the relay container's labels: %v", rs.Labels)
	}
	if len(d.Volumes) != 1 || d.Volumes[0].Name != appLinkVolume("R") || d.Volumes[0].Labels[runLabel] != "R" {
		t.Errorf("the relay volume and its labels: %+v", d.Volumes)
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
		if c.Op == "Stop" || c.Op == "RemoveForce" || c.Op == "VolumeRemove" {
			order = append(order, c.Op+" "+c.Arg)
		}
	}
	if !slices.Equal(order, []string{"Stop " + containerName("R", "applink-relay"), "RemoveForce " + containerName("R", "applink-relay"), "VolumeRemove " + appLinkVolume("R")}) {
		t.Errorf("teardown order: %v", order)
	}

	// A run whose daemon runs here has no relay volume, and starts nothing.
	none := &dockerx.Fake{}
	if err := startAppLinkRelay(context.Background(), none, p, &runDirs{}, &teardown{d: none}, "R"); err != nil || len(none.Calls) != 0 {
		t.Errorf("a run with no relay: err %v, calls %+v", err, none.Calls)
	}

	// On a rootless engine the person is the relay's root already: no owner
	// is named and no capability is given back.
	d2, rd2, td2, _, _ := relayRig(t)
	rd2.engine.Rootless = true
	defer td2.run()
	if err := startAppLinkRelay(context.Background(), d2, p, rd2, td2, "R"); err != nil {
		t.Fatal(err)
	}
	var rs2 dockerx.RunSpec
	for _, c := range d2.Calls {
		if c.Op == "RunForeground" && c.Arg == containerName("R", "applink-relay") {
			rs2 = c.Run
		}
	}
	if !slices.Equal(rs2.Argv, []string{"applink-relay"}) || len(rs2.CapAdd) != 0 || !slices.Equal(rs2.CapDrop, []string{"ALL"}) {
		t.Errorf("the rootless relay: argv %v, caps added %v, dropped %v", rs2.Argv, rs2.CapAdd, rs2.CapDrop)
	}

	// A relay that ends before it is ready fails the start, classed as the
	// gateway's, naming the relay's own log; nothing is left running.
	t.Run("a relay that ends before it is ready", func(t *testing.T) {
		d, rd, td, _, _ := relayRig(t)
		defer td.run()
		d.Foreground["-applink-relay"] = &dockerx.FakeForeground{Handler: func(_ context.Context, _ dockerx.RunSpec, _ io.Reader, _, stderr io.Writer) (int, error) {
			_, _ = io.WriteString(stderr, "no such socket folder\n")
			return 1, nil
		}}
		d.LogsByName = map[string]string{containerName("R", "applink-relay"): "relay: cannot listen\n"}
		p := Params{Plan: &spec.Plan{Proxy: spec.ProxySpec{Image: "agentry-runner:test"}}}
		err := startAppLinkRelay(context.Background(), d, p, rd, td, "R")
		var ce *ClassifiedError
		if err == nil || !errors.As(err, &ce) || ce.Code != ExitGateway ||
			!strings.Contains(err.Error(), "the app link's relay did not start") || !strings.HasSuffix(err.Error(), ": relay: cannot listen") {
			t.Fatalf("start = %v", err)
		}
	})
}

// The app link's receipt line says how the supervisor is reached: its
// socket bound into the app link's container where the daemon runs here;
// the relay of the run's own, its volume and why where it runs in a VM.
func TestTheAppLinkTransportLineSaysHowTheSupervisorIsReached(t *testing.T) {
	sock := "/home/me/.agentry/runner/s/agent-assistant-app.sock"
	here := appLinkTransportLine(&runDirs{appLink: sock})
	if here != "the supervisor's socket "+sock+", bound into the app link's container by its exact path" {
		t.Errorf("here: %q", here)
	}
	vm := appLinkTransportLine(&runDirs{appLink: sock, appLinkRelay: appLinkVolume("R"), appLinkPlacement: "the daemon runs in a VM"})
	for _, want := range []string{"a relay of the run's own", "no network", "the volume " + appLinkVolume("R"), "to the supervisor's socket " + sock,
		"any other line is refused at the relay's socket", "the daemon runs in a VM"} {
		if !strings.Contains(vm, want) {
			t.Errorf("in a VM, the line lacks %q: %q", want, vm)
		}
	}
	if strings.Contains(vm, "bound into") {
		t.Errorf("in a VM, the line says the socket is bound: %q", vm)
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
	var answers, dropped syncBuffer
	b := &appLinkBridge{socket: sup2Sock, out: &answers, log: &dropped}
	done := make(chan error, 1)
	go func() { done <- b.start(context.Background(), inR, 5*time.Second) }()
	notCalls := []string{
		`{"id":1,"answer":{"text":"x"}}`,
		`{"id":2,"request":{"tool":"self_configure"},"cmd":"change"}`,
		`{"id":3,"request":{"tool":""}}`,
		`{"request":{"tool":"self_configure"}}`,
		`{"id":5,"request":{"tool":"self_configure"},"answer":{"text":"x"}}`,
		`{"id":6,"request":{"tool":"self_configure"},"ready":true}`,
	}
	lines := append([]string{`{"ready":true}`}, notCalls...)
	lines = append(lines, `{"id":4,"request":{"tool":"self_configure","arguments":{}}}`)
	for _, line := range lines {
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
	if n := strings.Count(dropped.String(), "a line from the relay that is not a tool call was dropped"); n != len(notCalls) {
		t.Errorf("%d lines were dropped, want the %d that are not calls: %q", n, len(notCalls), dropped.String())
	}
	if got := strings.Count(answers.String(), "\n"); got != 1 {
		t.Errorf("the relay was answered %d times: %q", got, answers.String())
	}

	// A call the supervisor cannot be reached for is answered as an error that
	// names why, so the model reads a refusal rather than silence.
	t.Run("a call the supervisor cannot be reached for", func(t *testing.T) {
		var answers syncBuffer
		b := &appLinkBridge{socket: filepath.Join(shortDir(t), "none.sock"), out: &answers, log: io.Discard}
		in := strings.NewReader(`{"ready":true}` + "\n" + `{"id":7,"request":{"tool":"self_configure"}}` + "\n")
		if err := b.start(context.Background(), in, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(answers.String(), `"id":7`) && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		f, err := applink.ReadFrame([]byte(strings.TrimSpace(answers.String())))
		if err != nil || f.ID != 7 || f.Answer == nil || !f.Answer.IsError || !strings.HasPrefix(f.Answer.Text, "Agentry did not answer: ") || len(f.Answer.Text) <= len("Agentry did not answer: ") {
			t.Fatalf("the relay was answered %q (%v)", answers.String(), err)
		}
	})

	// The bridge starts only on the relay's ready line, and says why it did not.
	t.Run("the bridge starts on the ready line alone", func(t *testing.T) {
		newBridge := func() *appLinkBridge {
			return &appLinkBridge{socket: filepath.Join(shortDir(t), "none.sock"), out: io.Discard, log: io.Discard}
		}
		err := newBridge().start(context.Background(), strings.NewReader(`{"id":1,"request":{"tool":"self_configure"}}`+"\n"), 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "its first line was not its ready line") {
			t.Errorf("a first line that is a call: %v", err)
		}
		if err := newBridge().start(context.Background(), strings.NewReader(""), 5*time.Second); !errors.Is(err, io.EOF) {
			t.Errorf("a relay that ended: %v", err)
		}
		silent, quiet := io.Pipe()
		defer func() { _ = quiet.Close() }()
		if err := newBridge().start(context.Background(), silent, time.Millisecond); err == nil || err.Error() != "no ready line" {
			t.Errorf("a relay that says nothing: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := newBridge().start(ctx, silent, time.Hour); !errors.Is(err, context.Canceled) {
			t.Errorf("a canceled start: %v", err)
		}
	})
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
	if got := (&runDirs{}).replacer().Replace(spec.PHAppLinkMount); got != "" {
		t.Errorf("with no app link, the mount is %q", got)
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

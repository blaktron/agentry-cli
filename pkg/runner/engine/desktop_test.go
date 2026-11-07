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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// standInLinkService is a link service on a socket, as Agentry Desktop will
// serve one: it binds a run, issues a token, lists the tools of the servers
// it knows, answers each call with the tool's name, refuses what it is told
// to, and records every message it was sent.
type standInLinkService struct {
	mu      sync.Mutex
	socket  string
	token   string
	tools   map[string][]desktoplink.Tool
	refuse  map[string]*desktop.Refusal // by tool
	binds   []desktop.BindRequest
	calls   []desktop.CallRequest
	unbinds []desktop.UnbindRequest
}

func (s *standInLinkService) Bind(_ context.Context, req desktop.BindRequest) (desktop.Bound, *desktop.Refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.binds = append(s.binds, req)
	b := desktop.Bound{Token: s.token, Servers: map[string][]desktoplink.Tool{}}
	for _, srv := range req.Servers {
		tools, ok := s.tools[srv.ID]
		if !ok {
			return desktop.Bound{}, &desktop.Refusal{Reason: receipt.DesktopNotInstalled, Server: srv.ID, Message: "Start the MCP server in " + srv.ID + "'s sidebar"}
		}
		b.Servers[srv.ID] = tools
	}
	return b, nil
}

func (s *standInLinkService) Call(_ context.Context, req desktop.CallRequest) (desktoplink.Answer, *desktop.Refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if req.Token != s.token {
		return desktoplink.Answer{}, &desktop.Refusal{Reason: receipt.DesktopUnavailable, Message: "not this run's token"}
	}
	if r := s.refuse[req.Tool]; r != nil {
		return desktoplink.Answer{}, r
	}
	return desktoplink.TextAnswer("answered "+req.Server+"/"+req.Tool, false), nil
}

func (s *standInLinkService) Unbind(_ context.Context, req desktop.UnbindRequest) *desktop.Refusal {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unbinds = append(s.unbinds, req)
	return nil
}

func (s *standInLinkService) sent() (binds []desktop.BindRequest, calls []desktop.CallRequest, unbinds []desktop.UnbindRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.binds), slices.Clone(s.calls), slices.Clone(s.unbinds)
}

var standInBlenderTools = []desktoplink.Tool{
	{Name: "get_scene_info", Description: "the scene", InputSchema: json.RawMessage(`{"type":"object"}`)},
	{Name: "execute_blender_code", Description: "run code", InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}}}`)},
	// A tool the installed server lists that the catalogue withholds: the
	// run is not given it.
	{Name: "disable_telemetry", Description: "withheld"},
}

func serveStandIn(t *testing.T, dir string) *standInLinkService {
	t.Helper()
	s := &standInLinkService{socket: filepath.Join(dir, "desktop.sock"), token: "tok-standin", tools: map[string][]desktoplink.Tool{"blender": standInBlenderTools}, refuse: map[string]*desktop.Refusal{}}
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = ln.Close() })
	go func() { _ = desktop.Serve(ctx, ln, s) }()
	return s
}

// blenderPlan is a plan whose release uses Blender on the person's computer.
func blenderPlan() *spec.Plan {
	return &spec.Plan{
		Proxy: spec.ProxySpec{Image: "agentry-runner:test"},
		Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{Servers: []spec.DesktopServerCap{{
			ID: "blender", Application: "Blender", Package: "pypi:mcp-for-blender@2.2.7",
			Tools:  []spec.DesktopTool{{Name: "execute_blender_code", Class: "exec"}, {Name: "get_scene_info", Class: "read"}, {Name: "look", Class: "read"}},
			Egress: []string{"api.polyhaven.com"},
		}}}},
	}
}

// desktopRig is a run bound to the stand-in over its socket, as a run from
// the CLI binds: the fake engine plays the relay container with the real
// relay, its socket at relaySock.
func desktopRig(t *testing.T) (d *dockerx.Fake, p Params, rd *runDirs, td *teardown, env *receipt.Envelope, s *standInLinkService, relaySock string) {
	t.Helper()
	dir := shortDir(t)
	s = serveStandIn(t, dir)
	p = Params{Plan: blenderPlan()}
	c := desktop.Client{Socket: s.socket}
	bound, err := c.Bind(context.Background(), desktop.BindRequest{Run: "R", Ref: "acme/scene@1.0.0", Servers: []desktop.BindServer{{ID: "blender", Tools: []string{"execute_blender_code", "get_scene_info", "look"}}}})
	if err != nil {
		t.Fatal(err)
	}
	p.Desktop = BoundDesktopLink(c, "R", bound)
	rd = &runDirs{root: filepath.Join(dir, "R"), receiptsDir: filepath.Join(dir, "rc")}
	if err := os.MkdirAll(rd.receiptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	relaySock = filepath.Join(dir, "link.sock")
	d = &dockerx.Fake{Foreground: map[string]*dockerx.FakeForeground{
		"-" + desktoplink.RelayRole: {Handler: func(ctx context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			if err := desktoplink.Relay(ctx, desktoplink.RelayConfig{Listen: relaySock, In: stdin, Out: stdout, Log: stderr}); err != nil {
				return 1, err
			}
			return 0, nil
		}},
	}}
	td = &teardown{d: d}
	env = &receipt.Envelope{CapabilitiesEnforced: map[string]string{}}
	if err := prepareDesktopLink(p, rd, "R"); err != nil {
		t.Fatal(err)
	}
	recordDesktop(rd, td, env)
	return d, p, rd, td, env, s, relaySock
}

// A bound run's call reaches the stand-in link service and its answer the
// agent: the relay runs on no network with a read-only root, holding the
// run's relay volume alone, on the daemon's default runtime whatever the
// policy's (the socket it makes must land on the volume), attached to this
// process by its stdio; the binding it is handed lists the catalogue's tools
// alone; the call crosses with the run token and is kept as a receipt line
// with its class; teardown unbinds and removes the container before the
// volume.
func TestTheDesktopLinkCarriesACallToTheLinkService(t *testing.T) {
	d, p, rd, td, env, s, relaySock := desktopRig(t)
	p.Plan.Docker = &spec.DockerSpec{Runtime: "runsc", GatewayRuntime: "runsc-uds"}
	if err := startDesktopLinkRelay(context.Background(), d, p, rd, td, "R"); err != nil {
		t.Fatal(err)
	}
	var rs dockerx.RunSpec
	for _, c := range d.Calls {
		if c.Op == "RunForeground" && c.Arg == containerName("R", desktoplink.RelayRole) {
			rs = c.Run
		}
	}
	if rs.Name == "" {
		t.Fatalf("no relay container: %+v", d.Calls)
	}
	if rs.Network != "none" || rs.User != "0" || !rs.ReadOnly || !rs.Interactive || rs.Image != "agentry-runner:test" || rs.Runtime != "" ||
		!slices.Equal(rs.CapDrop, []string{"ALL"}) || !slices.Equal(rs.CapAdd, []string{"CHOWN"}) {
		t.Errorf("the relay's posture: %+v", rs)
	}
	if len(d.Volumes) != 1 || d.Volumes[0].Name != desktopVolume("R") || d.Volumes[0].Labels[runLabel] != "R" {
		t.Errorf("the relay volume and its labels: %+v", d.Volumes)
	}
	if len(rs.Mounts) != 1 || rs.Mounts[0] != (dockerx.Mount{Source: desktopVolume("R"), Target: desktoplink.SocketDir}) {
		t.Errorf("the relay's mounts: %+v", rs.Mounts)
	}
	// The binding the link entries read: the token, and the tools the
	// catalogue gives, in the plan's order; never the withheld tool.
	bind, err := desktoplink.ReadBind(filepath.Join(filepath.Dir(relaySock), filepath.Base(desktoplink.BindFile)))
	if err != nil || bind.Token != "tok-standin" {
		t.Fatalf("the binding: %+v, %v", bind, err)
	}
	var names []string
	for _, tool := range bind.Servers["blender"] {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{"execute_blender_code", "get_scene_info"}) {
		t.Fatalf("the binding lists %v", names)
	}

	a, err := desktoplink.Send(context.Background(), relaySock, desktoplink.Request{Token: "tok-standin", Server: "blender", Tool: "get_scene_info", Arguments: json.RawMessage(`{"detail":"full"}`)})
	if err != nil || a.IsError || a.Text() != "answered blender/get_scene_info" {
		t.Fatalf("answer %+v, %v", a, err)
	}
	_, calls, _ := s.sent()
	if len(calls) != 1 || calls[0].Run != "R" || calls[0].Token != "tok-standin" || calls[0].Server != "blender" || calls[0].Tool != "get_scene_info" || string(calls[0].Arguments) != `{"detail":"full"}` {
		t.Fatalf("the link service was sent %+v", calls)
	}
	// The receipt: one line a call, with its class.
	log, err := os.ReadFile(filepath.Join(rd.receiptsDir, desktopLogFile("blender")))
	if err != nil {
		t.Fatal(err)
	}
	var line desktopLogLine
	if err := json.Unmarshal(log, &line); err != nil || line.Tool != "get_scene_info" || line.Class != "read" || line.Outcome != "ok" || string(line.Arguments) != `{"detail":"full"}` {
		t.Fatalf("the receipt line: %s (%v)", log, err)
	}
	if line := env.CapabilitiesEnforced["desktop.transport"]; !strings.Contains(line, desktopVolume("R")) || !strings.Contains(line, s.socket) || !strings.Contains(line, "no socket of Agentry Desktop's") {
		t.Errorf("desktop.transport: %q", line)
	}

	d.Calls = nil
	td.run()
	var order []string
	for _, c := range d.Calls {
		if c.Op == "Stop" || c.Op == "RemoveForce" || c.Op == "VolumeRemove" {
			order = append(order, c.Op+" "+c.Arg)
		}
	}
	if !slices.Equal(order, []string{"Stop " + containerName("R", desktoplink.RelayRole), "RemoveForce " + containerName("R", desktoplink.RelayRole), "VolumeRemove " + desktopVolume("R")}) {
		t.Errorf("teardown order: %v", order)
	}
	_, _, unbinds := s.sent()
	if len(unbinds) != 1 || unbinds[0].Run != "R" || unbinds[0].Token != "tok-standin" {
		t.Errorf("unbound %+v", unbinds)
	}
	line2 := env.CapabilitiesEnforced["desktop.blender"]
	for _, want := range []string{"Blender", "pypi:mcp-for-blender@2.2.7", "agentry-desktop-blender", "1 calls (1 read)", "api.polyhaven.com", "not enforced", "receipts/desktop-blender.log"} {
		if !strings.Contains(line2, want) {
			t.Errorf("desktop.blender lacks %q: %q", want, line2)
		}
	}

	// A run with no desktop line starts nothing.
	none := &dockerx.Fake{}
	if err := startDesktopLinkRelay(context.Background(), none, Params{Plan: &spec.Plan{}}, &runDirs{}, &teardown{d: none}, "R"); err != nil || len(none.Calls) != 0 {
		t.Errorf("a run with no desktop link: err %v, calls %+v", err, none.Calls)
	}
}

// Nothing but a tool call of a bound server carrying the run token crosses
// the link, at either end: a frame that is not a call, one naming a server
// the run did not bind, one with another token and one for a tool the
// catalogue does not give are dropped by the relay at its socket and by the
// bridge at its stdio, and the link service sees none of them; a refusal
// from the link service reaches the agent as an error naming the reason and
// is counted by it.
func TestTheDesktopLinkCarriesNothingButToolCalls(t *testing.T) {
	d, p, rd, td, env, s, relaySock := desktopRig(t)
	defer td.run()
	s.refuse["execute_blender_code"] = &desktop.Refusal{Reason: receipt.DesktopPaused, Message: "the person paused the agent's use of Blender"}
	if err := startDesktopLinkRelay(context.Background(), d, p, rd, td, "R"); err != nil {
		t.Fatal(err)
	}
	for name, line := range map[string]string{
		"another token":         `{"token":"tok-other","server":"blender","tool":"get_scene_info"}`,
		"an unbound server":     `{"token":"tok-standin","server":"unreal","tool":"call_tool"}`,
		"a withheld tool":       `{"token":"tok-standin","server":"blender","tool":"disable_telemetry"}`,
		"a supervisor line":     `{"cmd":"stop"}`,
		"an answer, not a call": `{"id":1,"answer":{"content":[]}}`,
	} {
		c, err := net.Dial("unix", relaySock)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte(line + "\n"))
		sc := bufio.NewScanner(c)
		if !sc.Scan() || !strings.Contains(sc.Text(), `"isError":true`) {
			t.Errorf("%s: answered %q", name, sc.Text())
		}
		_ = c.Close()
	}
	if _, calls, _ := s.sent(); len(calls) != 0 {
		t.Fatalf("the link service was sent %+v", calls)
	}

	// A refused call: the reason reaches the agent and the receipt.
	a, err := desktoplink.Send(context.Background(), relaySock, desktoplink.Request{Token: "tok-standin", Server: "blender", Tool: "execute_blender_code", Arguments: json.RawMessage(`{"code":"import bpy"}`)})
	if err != nil || !a.IsError || a.Text() != "the person paused the agent's use of Blender" {
		t.Fatalf("a refused call was answered %+v, %v", a, err)
	}
	log, _ := os.ReadFile(filepath.Join(rd.receiptsDir, desktopLogFile("blender")))
	if !strings.Contains(string(log), `"class":"exec"`) || !strings.Contains(string(log), `"outcome":"refused:desktop-paused"`) {
		t.Fatalf("the receipt line: %s", log)
	}
	if line := rd.desktop.receiptLine(p.Plan.Capabilities.Desktop.Servers[0]); !strings.Contains(line, "1 calls (1 exec)") || !strings.Contains(line, "refused: 1 desktop-paused") {
		t.Errorf("the receipt's line: %q", line)
	}
	_ = env

	// The bridge alone: frames from a relay that are not calls of the run's.
	t.Run("the bridge drops what is not the run's call", func(t *testing.T) {
		dir := shortDir(t)
		s2 := serveStandIn(t, dir)
		c := desktop.Client{Socket: s2.socket}
		bound, err := c.Bind(context.Background(), desktop.BindRequest{Run: "R2", Servers: []desktop.BindServer{{ID: "blender"}}})
		if err != nil {
			t.Fatal(err)
		}
		run := &desktopRun{link: BoundDesktopLink(c, "R2", bound), volume: desktopVolume("R2"), calls: map[string]*desktopCounts{}}
		run.servers = []desktopServer{{DesktopServerCap: blenderPlan().Capabilities.Desktop.Servers[0], tools: standInBlenderTools[:2]}}
		run.calls["blender"] = &desktopCounts{byClass: map[string]int{}, refused: map[string]int{}}
		inR, inW := io.Pipe()
		var answers, dropped syncBuffer
		b := &desktopBridge{run: run, runID: "R2", out: &answers, log: &dropped}
		done := make(chan error, 1)
		go func() { done <- b.start(context.Background(), inR, 5*time.Second) }()
		notCalls := []string{
			`{"id":1,"answer":{"content":[]}}`,
			`{"id":2,"server":"blender","token":"tok-standin","request":{"tool":"get_scene_info"},"cmd":"change"}`,
			`{"id":3,"server":"blender","token":"tok-standin","request":{"tool":""}}`,
			`{"server":"blender","token":"tok-standin","request":{"tool":"get_scene_info"}}`,
			`{"id":5,"server":"blender","token":"tok-standin","request":{"tool":"get_scene_info"},"answer":{"content":[]}}`,
			`{"id":6,"server":"blender","token":"tok-standin","request":{"tool":"get_scene_info"},"ready":true}`,
			`{"id":7,"server":"blender","request":{"tool":"get_scene_info"}}`,
			`{"id":8,"server":"blender","token":"tok-other","request":{"tool":"get_scene_info"}}`,
			`{"id":9,"server":"unreal","token":"tok-standin","request":{"tool":"call_tool"}}`,
			`{"id":10,"server":"blender","token":"tok-standin","request":{"tool":"disable_telemetry"}}`,
		}
		lines := append([]string{`{"ready":true}`}, notCalls...)
		lines = append(lines, `{"id":4,"server":"blender","token":"tok-standin","request":{"tool":"get_scene_info","arguments":{}}}`)
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
		_, calls, _ := s2.sent()
		if len(calls) != 1 || calls[0].Tool != "get_scene_info" || calls[0].Token != "tok-standin" || calls[0].Run != "R2" {
			t.Fatalf("the link service was sent %+v", calls)
		}
		if !strings.Contains(answers.String(), `{"id":4,"answer":{"content":[{"type":"text","text":"answered blender/get_scene_info"}]}}`) {
			t.Fatalf("the relay was answered %q", answers.String())
		}
		if n := strings.Count(dropped.String(), "was dropped"); n != len(notCalls) {
			t.Errorf("%d lines were dropped, want the %d that are not the run's calls: %q", n, len(notCalls), dropped.String())
		}
		if got := strings.Count(answers.String(), "\n"); got != 1 {
			t.Errorf("the relay was answered %d times: %q", got, answers.String())
		}
	})

	// A call the link service cannot be reached for is answered as an error
	// that names why, so the model reads a refusal rather than silence.
	t.Run("a call the link service cannot be reached for", func(t *testing.T) {
		run := &desktopRun{link: BoundDesktopLink(desktop.Client{Socket: filepath.Join(shortDir(t), "none.sock")}, "R3", desktop.Bound{Token: "t"}), calls: map[string]*desktopCounts{}}
		run.servers = []desktopServer{{DesktopServerCap: blenderPlan().Capabilities.Desktop.Servers[0], tools: standInBlenderTools[:2]}}
		run.calls["blender"] = &desktopCounts{byClass: map[string]int{}, refused: map[string]int{}}
		var answers syncBuffer
		b := &desktopBridge{run: run, runID: "R3", out: &answers, log: io.Discard}
		in := strings.NewReader(`{"ready":true}` + "\n" + `{"id":7,"server":"blender","token":"t","request":{"tool":"get_scene_info"}}` + "\n")
		if err := b.start(context.Background(), in, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(answers.String(), `"id":7`) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		f, err := desktoplink.ReadFrame([]byte(strings.TrimSpace(answers.String())))
		if err != nil || f.ID != 7 || f.Answer == nil || !f.Answer.IsError || !strings.HasPrefix(f.Answer.Text(), "Agentry Desktop did not answer: ") {
			t.Fatalf("the relay was answered %q (%v)", answers.String(), err)
		}
		if line := run.receiptLine(run.servers[0].DesktopServerCap); !strings.Contains(line, "1 not answered") {
			t.Errorf("the receipt's line: %q", line)
		}
	})
}

// The desktop link's containers have no network: the relay joins none, the
// entries' catalog mount is the relay volume at the socket's folder and
// nothing of this computer, and the run's docker filter is told the volume,
// the image and each entry's command so it holds them to that.
func TestTheDesktopLinkHasNoNetwork(t *testing.T) {
	d, p, rd, td, _, _, _ := desktopRig(t)
	defer td.run()
	if err := startDesktopLinkRelay(context.Background(), d, p, rd, td, "R"); err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Calls {
		if c.Op == "RunForeground" && c.Run.Network != "none" {
			t.Errorf("the relay joined %q", c.Run.Network)
		}
		if c.Op == "NetworkConnect" {
			t.Errorf("a network was connected: %+v", c)
		}
	}
	rep := rd.replacer()
	if got := rep.Replace(spec.PHDesktopMount); got != desktopVolume("R")+":"+desktoplink.SocketDir+":rw" {
		t.Errorf("the entries' mount is %q", got)
	}
	if got := (&runDirs{}).replacer().Replace(spec.PHDesktopMount); got != "" {
		t.Errorf("with no desktop link, the mount is %q", got)
	}
	p.Plan.Docker = &spec.DockerSpec{Runtime: "runsc", GatewayRuntime: "runsc-uds"}
	cfg, err := filterConfig(rd, "R", map[string]string{}, p.limits(), p.Plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DesktopVolume != desktopVolume("R") || cfg.DesktopImage != "agentry-runner:test" || len(cfg.DesktopCmds) != 1 || !slices.Equal(cfg.DesktopCmds[0], []string{"desktoplink", "--server", "blender"}) || cfg.DesktopRuntime != "runsc-uds" {
		t.Errorf("the filter's desktop link: %q %q %v %q", cfg.DesktopVolume, cfg.DesktopImage, cfg.DesktopCmds, cfg.DesktopRuntime)
	}
	// The entries open the relay's socket in the volume, a host socket to a
	// sandbox: a policy runtime with no gatewayRuntime, or podman under a
	// runtime, is refused before the relay starts; no runtime at all is fine.
	if err := desktopRuntimeCheck(Params{Plan: &spec.Plan{Docker: &spec.DockerSpec{Runtime: "runsc"}}}, dockerx.Engine{}); err == nil || !strings.Contains(err.Error(), "docker.gatewayRuntime") {
		t.Errorf("a runtime without gatewayRuntime: %v", err)
	}
	if err := desktopRuntimeCheck(Params{Plan: &spec.Plan{Docker: &spec.DockerSpec{Runtime: "runsc", GatewayRuntime: "runsc-uds"}}}, dockerx.Engine{Kind: dockerx.KindPodman}); err == nil || !strings.Contains(err.Error(), "podman") {
		t.Errorf("podman under a runtime: %v", err)
	}
	if err := desktopRuntimeCheck(Params{Plan: &spec.Plan{Docker: &spec.DockerSpec{Runtime: "runsc", GatewayRuntime: "runsc-uds"}}}, dockerx.Engine{}); err != nil {
		t.Errorf("docker with both: %v", err)
	}
	if err := desktopRuntimeCheck(Params{Plan: &spec.Plan{}}, dockerx.Engine{Kind: dockerx.KindPodman}); err != nil {
		t.Errorf("no runtime: %v", err)
	}
	if line := runtimeLine(p); !strings.Contains(line, "the desktop link's entries") || !strings.Contains(line, "the relay that makes that socket runs on the daemon's default runtime") {
		t.Errorf("the runtime line: %q", line)
	}
	none, err := filterConfig(&runDirs{}, "R", map[string]string{}, p.limits(), &spec.Plan{}, nil)
	if err != nil || none.DesktopVolume != "" || none.DesktopImage != "" || none.DesktopCmds != nil {
		t.Errorf("with no desktop link, the filter is told %q %q %v (%v)", none.DesktopVolume, none.DesktopImage, none.DesktopCmds, err)
	}
}

// A run that declares the person's computer is refused before anything is
// made where it cannot reach it: with no binding (no Agentry Desktop
// answering), exit 11 naming the application and Agentry Desktop; with a
// binding that leaves a declared server out; and a binding given to a run
// that declares none.
func TestARunNeedsAgentryDesktop(t *testing.T) {
	plan := blenderPlan()
	err := checkDesktop(Params{Plan: plan})
	var ce *ClassifiedError
	if err == nil || !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "Blender on your computer") || !strings.Contains(err.Error(), "Agentry Desktop running") {
		t.Fatalf("no binding: %v", err)
	}
	if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonDesktopRefused || r.Detail != receipt.DesktopUnavailable || !slices.Equal(r.Names, []string{"blender"}) {
		t.Fatalf("no binding's reason: %+v", r)
	}
	partial := &DesktopLink{Token: "t", Servers: map[string][]desktoplink.Tool{"unreal": nil}, Call: func(context.Context, desktop.CallRequest) (desktoplink.Answer, error) {
		return desktoplink.Answer{}, nil
	}}
	err = checkDesktop(Params{Plan: plan, Desktop: partial})
	if r := receipt.ReasonOf(err); err == nil || r == nil || r.Detail != receipt.DesktopNotInstalled || !slices.Equal(r.Names, []string{"blender"}) {
		t.Fatalf("a binding without the server: %v, %+v", err, r)
	}
	if err := checkDesktop(Params{Plan: &spec.Plan{}, Desktop: partial}); err == nil || !strings.Contains(err.Error(), "declares no application") {
		t.Fatalf("a binding for a plan without the line: %v", err)
	}
	if err := checkDesktop(Params{Plan: &spec.Plan{}}); err != nil {
		t.Fatalf("a plan without the line: %v", err)
	}
	full := &DesktopLink{Token: "t", Servers: map[string][]desktoplink.Tool{"blender": nil}, Call: partial.Call}
	if err := checkDesktop(Params{Plan: plan, Desktop: full}); err != nil {
		t.Fatalf("a full binding: %v", err)
	}
	// A refusal from the link service at bind carries its reason and its
	// server; one outside the closed list reads as not installed.
	err = DesktopBindRefusal(plan.Capabilities.Desktop, &desktop.Refusal{Reason: receipt.DesktopNotInstalled, Server: "blender", Message: "Start the MCP server in Blender's sidebar"})
	if r := receipt.ReasonOf(err); err == nil || err.Error() != "Start the MCP server in Blender's sidebar" || r.Detail != receipt.DesktopNotInstalled || !slices.Equal(r.Names, []string{"blender"}) {
		t.Fatalf("a bind refusal: %v, %+v", err, r)
	}
	if r := receipt.ReasonOf(DesktopBindRefusal(plan.Capabilities.Desktop, &desktop.Refusal{Reason: "something-else"})); r.Detail != receipt.DesktopNotInstalled {
		t.Fatalf("an unknown reason: %+v", r)
	}
}

// A hosted run never has the person's computer: it is refused before
// anything is made, as hosted, naming the application servers.
func TestHostedRunsRefuseTheDesktopLine(t *testing.T) {
	plan := blenderPlan()
	full := &DesktopLink{Token: "t", Servers: map[string][]desktoplink.Tool{"blender": nil}, Call: func(context.Context, desktop.CallRequest) (desktoplink.Answer, error) {
		return desktoplink.Answer{}, nil
	}}
	for name, p := range map[string]Params{
		"marked hosted":        {Plan: plan, Hosted: true, Desktop: full},
		"reviewing its inputs": {Plan: plan, InputReview: &fakeReview{}, Desktop: full},
		"hosted, unbound":      {Plan: plan, Hosted: true},
	} {
		err := checkDesktop(p)
		var ce *ClassifiedError
		if err == nil || !errors.As(err, &ce) || ce.Code != ExitPolicy || !strings.Contains(err.Error(), "a hosted run never reaches your computer") {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonDesktopRefused || r.Detail != receipt.DesktopHosted || !slices.Equal(r.Names, []string{"blender"}) {
			t.Errorf("%s: reason %+v", name, r)
		}
	}
	if err := checkDesktop(Params{Plan: &spec.Plan{}, Hosted: true}); err != nil {
		t.Errorf("a hosted run without the line: %v", err)
	}
}

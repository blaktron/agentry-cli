package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// The install consent names each application the release uses on the
// person's computer by its id, how the release uses it (its MCP server,
// with every tool it lists or the tools it narrows to), and computer use
// by the windows it sees. The gate asks about an application on the
// computer, and about the screen, in words the desktop and the site copy.
func TestTheConsentNamesTheDesktopLine(t *testing.T) {
	c := manifest.Capabilities{Desktop: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender"}}}}
	lines := strings.Join(capabilityLines(c), "\n")
	want := "desktop: the application blender on your computer, through its MCP server (every tool it lists)"
	if !strings.Contains(lines, want) {
		t.Errorf("consent %q lacks %q", lines, want)
	}
	if strings.Contains(lines, "computerUse") {
		t.Errorf("consent %q names computer use the release does not declare", lines)
	}
	if !capabilityNeedsConfirm(c) || !slices.Equal(confirmedCapabilities(c), []string{"an application on your computer"}) || isZeroCapabilities(c) {
		t.Errorf("the application is not asked about: %v", confirmedCapabilities(c))
	}

	narrowed := manifest.Capabilities{Desktop: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender", Tools: []string{"get_scene_info", "look"}}}}}
	if l := strings.Join(capabilityLines(narrowed), "\n"); !strings.Contains(l, "desktop: the application blender on your computer, through its MCP server (tools get_scene_info, look)") {
		t.Errorf("a narrowed server's consent: %q", l)
	}

	two := manifest.Capabilities{Desktop: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender"}, {ID: "unreal"}}, ComputerUse: manifest.ComputerUseApp}}
	if got := confirmedCapabilities(two); !slices.Equal(got, []string{"applications on your computer", "your screen"}) {
		t.Errorf("two applications and computer use are asked about as %v", got)
	}
	if l := strings.Join(capabilityLines(two), "\n"); !strings.Contains(l, "desktop.computerUse: can see and use the windows of blender and unreal on your computer") {
		t.Errorf("computer use's consent: %q", l)
	}
	screen := manifest.Capabilities{Desktop: &manifest.Desktop{ComputerUse: manifest.ComputerUseScreen}}
	if got := confirmedCapabilities(screen); !slices.Equal(got, []string{"your screen"}) {
		t.Errorf("the screen alone is asked about as %v", got)
	}
	if l := strings.Join(capabilityLines(screen), "\n"); !strings.Contains(l, "desktop.computerUse: can see and use your whole screen") {
		t.Errorf("the screen's consent: %q", l)
	}
	if got := confirmedCapabilitiesWords(manifest.Capabilities{Exec: manifest.ExecCapability{Commands: []string{"git"}}, Desktop: two.Desktop}); got != "exec, applications on your computer and your screen" {
		t.Errorf("the words: %q", got)
	}
}

func desktopPlan() *spec.Plan {
	return &spec.Plan{Ref: "acme/scene@1.0.0", Digest: "sha256:beef", Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{Servers: []spec.DesktopServerCap{{
		ID: "blender", Application: "blender",
	}}, PersonServers: spec.PersonServersAllowed}}}
}

// A run of a release that uses the person's computer is refused before the
// sandbox, exit 11, naming the application and Agentry Desktop, when no
// Agentry Desktop answers on this computer: no socket in the runner home, a
// file where the socket would be, or a socket nothing listens on. With one
// answering, the run is bound and carries the service's token and tools;
// a refusal from the service carries its reason.
func TestARunNeedsAgentryDesktop(t *testing.T) {
	dir, err := os.MkdirTemp("", "ad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, "s"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := desktopPlan()
	refused := func(t *testing.T, name string, link *engine.DesktopLink, err error) {
		t.Helper()
		var ce *engine.ClassifiedError
		if link != nil || err == nil || !errors.As(err, &ce) || ce.Code != engine.ExitPolicy ||
			!strings.Contains(err.Error(), "blender on your computer") || !strings.Contains(err.Error(), "needs Agentry Desktop running on this computer") {
			t.Fatalf("%s: link %+v, err %v", name, link, err)
		}
		if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonDesktopRefused || r.Detail != receipt.DesktopUnavailable || !slices.Equal(r.Names, []string{"blender"}) {
			t.Fatalf("%s: reason %+v", name, r)
		}
	}
	link, err := bindDesktop(context.Background(), plan, dir, "R", "", runOpts{})
	refused(t, "no socket", link, err)
	sock := desktop.SocketPath(dir)
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link, err = bindDesktop(context.Background(), plan, dir, "R", "", runOpts{})
	refused(t, "a file where the socket would be", link, err)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	link, err = bindDesktop(context.Background(), plan, dir, "R", "", runOpts{})
	refused(t, "a socket nothing listens on", link, err)
	_ = os.Remove(sock)

	// A plan without the line binds nothing.
	if link, err := bindDesktop(context.Background(), &spec.Plan{}, dir, "R", "", runOpts{}); link != nil || err != nil {
		t.Fatalf("a plan without the line: %+v, %v", link, err)
	}

	// Agentry Desktop answering: the run is bound with its token and tools,
	// and the bind named the run, the release and the catalogue's tools.
	s := &standInService{token: "tok-1", tools: map[string][]desktoplink.Tool{"blender": {{Name: "get_scene_info"}, {Name: "execute_blender_code"}}}}
	ln, err = net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = desktop.Serve(ctx, ln, s) }()
	link, err = bindDesktop(context.Background(), plan, dir, "R", "", runOpts{})
	if err != nil || link == nil || link.Token != "tok-1" || len(link.Servers["blender"]) != 2 || !strings.Contains(link.FarEnd, sock) {
		t.Fatalf("bound: %+v, %v", link, err)
	}
	if len(s.binds) != 1 || s.binds[0].Run != "R" || s.binds[0].Ref != "acme/scene@1.0.0" || len(s.binds[0].Servers) != 1 ||
		s.binds[0].Servers[0].ID != "blender" || len(s.binds[0].Servers[0].Tools) != 0 || s.binds[0].Protocol != desktop.Protocol {
		t.Fatalf("the bind sent %+v", s.binds)
	}
	if err := link.Unbind(context.Background()); err != nil || len(s.unbinds) != 1 || s.unbinds[0].Token != "tok-1" {
		t.Fatalf("unbind: %v, %+v", err, s.unbinds)
	}
	if s.binds[0].ComputerUse != "" || len(s.binds[0].ComputerTools) != 0 || link.Computer != nil {
		t.Fatalf("a run without computer use asked for it: %+v", s.binds[0])
	}

	// Computer use: the bind asks for it in the release's scope with the
	// run's computer-use tools, and the run carries what was bound.
	cu := desktopPlan()
	cu.Capabilities.Desktop.ComputerUse = "app"
	cu.Capabilities.Desktop.ComputerTools = []spec.DesktopTool{{Name: "screenshot", Class: "read"}, {Name: "left_click", Class: "act"}}
	link, err = bindDesktop(context.Background(), cu, dir, "R", "/out/R", runOpts{})
	if err != nil || link.Computer == nil || link.Computer.Scope != "app" {
		t.Fatalf("computer use bound: %+v, %v", link, err)
	}
	if last := s.binds[len(s.binds)-1]; last.ComputerUse != "app" || !slices.Equal(last.ComputerTools, []string{"screenshot", "left_click"}) || last.Reports != filepath.Join("/out/R", "reports", "desktop") {
		t.Fatalf("the bind sent %+v", last)
	}

	// The service refusing the bind: its reason and its words reach the
	// person, exit 11.
	s.refuse = &desktop.Refusal{Reason: receipt.DesktopNotInstalled, Server: "blender", Message: "Start the MCP server in Blender's sidebar"}
	link, err = bindDesktop(context.Background(), plan, dir, "R", "", runOpts{})
	var ce *engine.ClassifiedError
	if link != nil || err == nil || !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || err.Error() != "Start the MCP server in Blender's sidebar" {
		t.Fatalf("a refused bind: %+v, %v", link, err)
	}
	if r := receipt.ReasonOf(err); r == nil || r.Detail != receipt.DesktopNotInstalled || !slices.Equal(r.Names, []string{"blender"}) {
		t.Fatalf("a refused bind's reason: %+v", r)
	}
}

// standInService is a link service for the bind's test.
type standInService struct {
	token   string
	tools   map[string][]desktoplink.Tool
	refuse  *desktop.Refusal
	binds   []desktop.BindRequest
	unbinds []desktop.UnbindRequest
}

func (s *standInService) Bind(_ context.Context, req desktop.BindRequest) (desktop.Bound, *desktop.Refusal) {
	s.binds = append(s.binds, req)
	if s.refuse != nil {
		return desktop.Bound{}, s.refuse
	}
	b := desktop.Bound{Token: s.token, Servers: s.tools}
	if req.ComputerUse != "" {
		b.Computer = &desktop.ComputerBound{Scope: req.ComputerUse, Enforced: "stand-in"}
	}
	return b, nil
}

func (s *standInService) Call(context.Context, desktop.CallRequest) (desktoplink.Answer, *desktop.Refusal) {
	return desktoplink.TextAnswer("answered", false), nil
}

func (s *standInService) Unbind(_ context.Context, req desktop.UnbindRequest) *desktop.Refusal {
	s.unbinds = append(s.unbinds, req)
	return nil
}

// A hosted run never has the person's computer: the CLI refuses it before
// anything is installed, exit 11, desktop-refused, hosted, by every sign of
// a hosted run; a run on this computer and a plan without the line pass.
func TestHostedRunsRefuseTheDesktopLine(t *testing.T) {
	plan := desktopPlan()
	for name, opts := range map[string]runOpts{
		"a connections file":   {connectionsFile: "/tmp/c.json"},
		"the session protocol": {sessionProtocol: true},
		"input review":         {reviewInputs: "https://dev.example"},
	} {
		err := refuseHostedDesktop(plan, opts)
		var ce *engine.ClassifiedError
		if err == nil || !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "a hosted run never reaches your computer") {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonDesktopRefused || r.Detail != receipt.DesktopHosted || !slices.Equal(r.Names, []string{"blender"}) {
			t.Errorf("%s: reason %+v", name, r)
		}
	}
	t.Setenv(envHosted, "1")
	if err := refuseHostedDesktop(plan, runOpts{}); err == nil {
		t.Error("a process marked hosted ran")
	}
	if err := refuseHostedDesktop(&spec.Plan{}, runOpts{}); err != nil {
		t.Errorf("a plan without the line: %v", err)
	}
	t.Setenv(envHosted, "")
	if err := refuseHostedDesktop(plan, runOpts{}); err != nil {
		t.Errorf("a run on this computer: %v", err)
	}
}

func TestTheRunSummaryNamesTheDesktopLine(t *testing.T) {
	if got := desktopSummary(desktopPlan().Capabilities.Desktop); got != "blender (every tool its server lists) on your computer, through Agentry Desktop" {
		t.Errorf("summary %q", got)
	}
	narrowed := desktopPlan().Capabilities.Desktop
	narrowed.Servers[0].Narrowed, narrowed.Servers[0].Tools = true, []spec.DesktopTool{{Name: "look"}}
	if got := desktopSummary(narrowed); got != "blender (1 tool) on your computer, through Agentry Desktop" {
		t.Errorf("summary %q", got)
	}
	cu := desktopPlan().Capabilities.Desktop
	cu.ComputerUse = "app"
	if got := desktopSummary(cu); got != "blender (every tool its server lists) and computer use in the windows you allow on your computer, through Agentry Desktop" {
		t.Errorf("summary %q", got)
	}
}

// A run on a runhost that uses this computer stays attached to it: a
// detached run and a schedule are refused before anything is bound, and a
// whole-screen scope needs what it needs locally.
func TestARunOnARunhostThatUsesThisComputerStaysAttached(t *testing.T) {
	m := &manifest.Manifest{Capabilities: manifest.Capabilities{Desktop: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender"}}}}}
	pol := policy.Default()
	for name, opts := range map[string]runOpts{"detach": {detach: true}, "schedule": {runhostSchedule: "{}"}} {
		if _, err := runOnDesktopCap(m, pol, opts); err == nil {
			t.Errorf("%s: not refused", name)
		}
	}
	d, err := runOnDesktopCap(m, pol, runOpts{})
	if err != nil || d == nil || len(d.Servers) != 1 || d.Servers[0].ID != "blender" {
		t.Fatalf("%+v %v", d, err)
	}
	screen := &manifest.Manifest{Capabilities: manifest.Capabilities{Desktop: &manifest.Desktop{ComputerUse: manifest.ComputerUseScreen}}}
	if _, err := runOnDesktopCap(screen, pol, runOpts{}); err == nil {
		t.Fatal("a whole-screen scope was allowed without --allow-screen")
	}
	if d, err := runOnDesktopCap(screen, pol, runOpts{allowScreen: true}); err != nil || d.ScreenAllowedBy == "" {
		t.Fatalf("with --allow-screen: %+v %v", d, err)
	}
	if d, err := runOnDesktopCap(&manifest.Manifest{}, pol, runOpts{detach: true}); d != nil || err != nil {
		t.Fatalf("a release with no desktop line: %+v %v", d, err)
	}
}

// The run's summary names each application by its id, and the chosen entry
// as "an application you choose".
func TestTheRunSummaryNamesTheChosenEntryToThePerson(t *testing.T) {
	d := &spec.DesktopCap{Servers: []spec.DesktopServerCap{
		{ID: "my-cad", Application: "my-cad", Narrowed: true, Tools: []spec.DesktopTool{{Name: "export_drawing"}}},
		{ID: spec.DesktopChosenID, Application: "an application the person chooses", Chosen: true},
	}, ComputerUse: spec.ComputerUseApp}
	want := "my-cad (1 tool), an application you choose (every tool its server lists) and computer use in the windows you allow on your computer, through Agentry Desktop"
	if got := desktopSummary(d); got != want {
		t.Errorf("summary %q, want %q", got, want)
	}
}

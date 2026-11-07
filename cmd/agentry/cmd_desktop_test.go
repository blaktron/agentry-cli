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

	"github.com/blaktron/agentry/pkg/appserver"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// The install consent names each application the release uses on the
// person's computer in the catalogue's words: the application, its tool
// count, how many tools run code inside it and how many hosts it reaches
// from the computer; a narrowed server counts its narrowed tools; computer
// use names the screen. The gate asks about an application on the computer,
// and about the screen, in words the desktop and the site copy.
func TestTheConsentNamesTheDesktopLine(t *testing.T) {
	blender, ok := appserver.Builtin().Lookup("blender")
	if !ok {
		t.Fatal("the catalogue has no blender entry")
	}
	c := manifest.Capabilities{Desktop: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender"}}}}
	lines := strings.Join(capabilityLines(c), "\n")
	want := "desktop: Blender on your computer through its MCP server (" + countWord(len(blender.Tools), "tool") + ", 1 runs code inside Blender; it reaches " + countWord(len(blender.Egress), "host") + " from your computer)"
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
	if l := strings.Join(capabilityLines(narrowed), "\n"); !strings.Contains(l, "desktop: Blender on your computer through its MCP server (2 tools)") {
		t.Errorf("a narrowed server's consent: %q", l)
	}

	two := manifest.Capabilities{Desktop: &manifest.Desktop{Servers: []manifest.DesktopServer{{ID: "blender"}, {ID: "unreal"}}, ComputerUse: manifest.ComputerUseApp}}
	if got := confirmedCapabilities(two); !slices.Equal(got, []string{"applications on your computer", "your screen"}) {
		t.Errorf("two applications and computer use are asked about as %v", got)
	}
	if l := strings.Join(capabilityLines(two), "\n"); !strings.Contains(l, "desktop.computerUse: can see and use the windows of Blender and unreal on your computer") {
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

func countWord(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strings.Join([]string{itoa(n), noun + "s"}, " ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func desktopPlan() *spec.Plan {
	return &spec.Plan{Ref: "acme/scene@1.0.0", Digest: "sha256:beef", Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{Servers: []spec.DesktopServerCap{{
		ID: "blender", Application: "Blender", Package: "pypi:mcp-for-blender@2.2.7",
		Tools: []spec.DesktopTool{{Name: "get_scene_info", Class: "read"}, {Name: "execute_blender_code", Class: "exec"}},
	}}}}}
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
			!strings.Contains(err.Error(), "Blender on your computer") || !strings.Contains(err.Error(), "needs Agentry Desktop running on this computer") {
			t.Fatalf("%s: link %+v, err %v", name, link, err)
		}
		if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonDesktopRefused || r.Detail != receipt.DesktopUnavailable || !slices.Equal(r.Names, []string{"blender"}) {
			t.Fatalf("%s: reason %+v", name, r)
		}
	}
	link, err := bindDesktop(context.Background(), plan, dir, "R", "")
	refused(t, "no socket", link, err)
	sock := desktop.SocketPath(dir)
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link, err = bindDesktop(context.Background(), plan, dir, "R", "")
	refused(t, "a file where the socket would be", link, err)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	link, err = bindDesktop(context.Background(), plan, dir, "R", "")
	refused(t, "a socket nothing listens on", link, err)
	_ = os.Remove(sock)

	// A plan without the line binds nothing.
	if link, err := bindDesktop(context.Background(), &spec.Plan{}, dir, "R", ""); link != nil || err != nil {
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
	link, err = bindDesktop(context.Background(), plan, dir, "R", "")
	if err != nil || link == nil || link.Token != "tok-1" || len(link.Servers["blender"]) != 2 || !strings.Contains(link.FarEnd, sock) {
		t.Fatalf("bound: %+v, %v", link, err)
	}
	if len(s.binds) != 1 || s.binds[0].Run != "R" || s.binds[0].Ref != "acme/scene@1.0.0" || len(s.binds[0].Servers) != 1 ||
		s.binds[0].Servers[0].ID != "blender" || !slices.Equal(s.binds[0].Servers[0].Tools, []string{"get_scene_info", "execute_blender_code"}) {
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
	link, err = bindDesktop(context.Background(), cu, dir, "R", "/out/R")
	if err != nil || link.Computer == nil || link.Computer.Scope != "app" {
		t.Fatalf("computer use bound: %+v, %v", link, err)
	}
	if last := s.binds[len(s.binds)-1]; last.ComputerUse != "app" || !slices.Equal(last.ComputerTools, []string{"screenshot", "left_click"}) || last.Reports != filepath.Join("/out/R", "reports", "desktop") {
		t.Fatalf("the bind sent %+v", last)
	}

	// The service refusing the bind: its reason and its words reach the
	// person, exit 11.
	s.refuse = &desktop.Refusal{Reason: receipt.DesktopNotInstalled, Server: "blender", Message: "Start the MCP server in Blender's sidebar"}
	link, err = bindDesktop(context.Background(), plan, dir, "R", "")
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
	if got := desktopSummary(desktopPlan().Capabilities.Desktop); got != "Blender (2 tools) on your computer, through Agentry Desktop" {
		t.Errorf("summary %q", got)
	}
	cu := desktopPlan().Capabilities.Desktop
	cu.ComputerUse = "app"
	if got := desktopSummary(cu); got != "Blender (2 tools) and computer use in the windows you allow on your computer, through Agentry Desktop" {
		t.Errorf("summary %q", got)
	}
}

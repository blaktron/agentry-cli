package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// Any application, set up from the library or by the person: the run takes
// each application's tools from its binding, narrows them where the release
// narrows them, and holds the person's own set-ups to the policy.

// bindStandIn binds plan's desktop line to a stand-in that answers from as
// where each application's set-up came from.
func bindStandIn(t *testing.T, plan *spec.Plan, from map[string]string) (*standInLinkService, *DesktopLink) {
	t.Helper()
	s := serveStandIn(t, shortDir(t))
	s.from = from
	c := desktop.Client{Socket: s.socket}
	bound, err := c.Bind(context.Background(), DesktopBindRequest(plan.Capabilities.Desktop, "R", "acme/scene@1.0.0", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	return s, BoundDesktopLink(c, "R", bound)
}

// The bind names the protocol, each application's narrowing (none for
// every tool its server lists), the chosen entry, and the windows of
// computer use.
func TestTheBindNamesWhatTheRunUses(t *testing.T) {
	d := &spec.DesktopCap{
		Servers: []spec.DesktopServerCap{
			{ID: "my-cad", Application: "my-cad", Narrowed: true, Tools: []spec.DesktopTool{{Name: "export_drawing"}}},
			{ID: spec.DesktopChosenID, Application: "an application the person chooses", Chosen: true},
		},
		ComputerUse: spec.ComputerUseApp, ComputerTools: []spec.DesktopTool{{Name: "screenshot", Class: "read"}},
		Windows: []string{"my-cad", "kdenlive"}, ChooseWindows: true,
	}
	req := DesktopBindRequest(d, "R", "acme/x@1.0.0", "", "")
	if req.Protocol != desktop.Protocol || len(req.Servers) != 2 || !slices.Equal(req.Servers[0].Tools, []string{"export_drawing"}) ||
		req.Servers[1].ID != spec.DesktopChosenID || !req.Servers[1].Chosen || len(req.Servers[1].Tools) != 0 ||
		!slices.Equal(req.Windows, []string{"my-cad", "kdenlive"}) || !req.ChooseWindows || req.ComputerUse != spec.ComputerUseApp {
		t.Fatalf("bind %+v", req)
	}
}

// An application with no narrowing has every tool its server listed, with
// the class Agentry Desktop gave each; a narrowing keeps its tools; a tool
// the narrowing names that the server does not list refuses the run before
// anything starts, desktop-not-installed naming the application.
func TestTheRunTakesItsToolsFromTheBinding(t *testing.T) {
	plan := blenderPlan()
	_, link := bindStandIn(t, plan, nil)
	rd := &runDirs{root: shortDir(t), receiptsDir: shortDir(t)}
	if err := prepareDesktopLink(Params{Plan: plan, Desktop: link}, rd, "R"); err != nil {
		t.Fatal(err)
	}
	b, _ := rd.desktop.server("blender")
	if got := b.ToolNames(); !slices.Equal(got, []string{"get_scene_info", "execute_blender_code", "look"}) {
		t.Fatalf("tools %v", got)
	}
	if tool, ok := b.tool("execute_blender_code"); !ok || tool.Class != "exec" {
		t.Fatalf("execute_blender_code = %+v, %v", tool, ok)
	}

	narrowed := blenderPlan()
	narrowed.Capabilities.Desktop.Servers[0].Narrowed = true
	narrowed.Capabilities.Desktop.Servers[0].Tools = []spec.DesktopTool{{Name: "look"}}
	_, link = bindStandIn(t, narrowed, nil)
	rd = &runDirs{root: shortDir(t), receiptsDir: shortDir(t)}
	if err := prepareDesktopLink(Params{Plan: narrowed, Desktop: link}, rd, "R"); err != nil {
		t.Fatal(err)
	}
	if b, _ := rd.desktop.server("blender"); !slices.Equal(b.ToolNames(), []string{"look"}) {
		t.Fatalf("narrowed tools %v", b.ToolNames())
	}

	missing := blenderPlan()
	missing.Capabilities.Desktop.Servers[0].Narrowed = true
	missing.Capabilities.Desktop.Servers[0].Tools = []spec.DesktopTool{{Name: "render_movie"}}
	link = &DesktopLink{Token: "t", Servers: map[string][]desktoplink.Tool{"blender": standInBlenderTools}, Call: func(context.Context, desktop.CallRequest) (desktoplink.Answer, error) {
		return desktoplink.Answer{}, nil
	}}
	err := checkDesktop(Params{Plan: missing, Desktop: link})
	if r := receipt.ReasonOf(err); err == nil || r == nil || r.Detail != receipt.DesktopNotInstalled || !slices.Equal(r.Names, []string{"blender"}) || !strings.Contains(err.Error(), "render_movie") {
		t.Fatalf("a narrowed tool the server does not list: %v, %+v", err, r)
	}
}

// A policy with desktop.allowCustomServers: false refuses an application
// whose set-up is the person's, desktop-not-granted, before anything starts;
// the library's own binds; under Unsafe Mode the person's binds and the
// receipt says Unsafe Mode allowed it. A binding with no From reads as the
// person's.
func TestThePolicyHoldsThePersonsOwnSetUps(t *testing.T) {
	for _, tc := range []struct {
		name, servers, from string
		refused             bool
		receipt             string
	}{
		{"the library, refused policy", spec.PersonServersRefused, desktop.FromLibrary, false, "set up from Agentry Desktop's library"},
		{"the person's, refused policy", spec.PersonServersRefused, desktop.FromPerson, true, ""},
		{"a changed preset, refused policy", spec.PersonServersRefused, desktop.FromLibraryEdited, true, ""},
		{"no From, refused policy", spec.PersonServersRefused, "", true, ""},
		{"the person's, Unsafe Mode", spec.PersonServersUnsafe, desktop.FromPerson, false, "not vetted by Agentry, allowed only because Unsafe Mode is on"},
		{"the person's, allowed", spec.PersonServersAllowed, desktop.FromPerson, false, "set up by the person in Agentry Desktop, not vetted by Agentry), answered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := blenderPlan()
			plan.Capabilities.Desktop.PersonServers = tc.servers
			from := map[string]string{"blender": tc.from}
			_, link := bindStandIn(t, plan, from)
			if tc.from == "" {
				link.From = nil
			}
			rd := &runDirs{root: shortDir(t), receiptsDir: shortDir(t)}
			err := prepareDesktopLink(Params{Plan: plan, Desktop: link}, rd, "R")
			if tc.refused {
				r := receipt.ReasonOf(err)
				if err == nil || r == nil || r.Detail != receipt.DesktopNotGranted || !strings.Contains(err.Error(), "desktop.allowCustomServers: false") {
					t.Fatalf("refusal %v, %+v", err, r)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if line := rd.desktop.receiptLine(rd.desktop.servers[0].DesktopServerCap); !strings.Contains(line, tc.receipt) {
				t.Fatalf("the receipt's line %q lacks %q", line, tc.receipt)
			}
		})
	}
}

// The chosen entry is named in the receipt by the application the person
// chose.
func TestTheChosenApplicationIsNamed(t *testing.T) {
	plan := &spec.Plan{Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{
		Servers: []spec.DesktopServerCap{{ID: spec.DesktopChosenID, Application: "an application the person chooses", Chosen: true}}, PersonServers: spec.PersonServersAllowed}}}
	link := &DesktopLink{Token: "t", Servers: map[string][]desktoplink.Tool{spec.DesktopChosenID: standInBlenderTools[:1]},
		From: map[string]string{spec.DesktopChosenID: desktop.FromPerson}, Chosen: &desktop.ChosenApplication{ID: "my-cad", Name: "My CAD"},
		Call: func(context.Context, desktop.CallRequest) (desktoplink.Answer, error) {
			return desktoplink.Answer{}, nil
		}}
	rd := &runDirs{root: shortDir(t), receiptsDir: shortDir(t)}
	if err := prepareDesktopLink(Params{Plan: plan, Desktop: link}, rd, "R"); err != nil {
		t.Fatal(err)
	}
	if line := rd.desktop.receiptLine(rd.desktop.servers[0].DesktopServerCap); !strings.HasPrefix(line, "My CAD (the application the person chose) on the person's computer") ||
		!strings.Contains(line, "agentry-desktop-chosen") {
		t.Fatalf("the receipt's line %q", line)
	}
}

// The gateway's allowlist for a desktop link entry is set from the binding
// when the run renders its gateway files.
func TestTheAllowlistIsTheBindingsTools(t *testing.T) {
	plan := blenderPlan()
	_, link := bindStandIn(t, plan, nil)
	rd := &runDirs{root: shortDir(t), receiptsDir: shortDir(t)}
	if err := prepareDesktopLink(Params{Plan: plan, Desktop: link}, rd, "R"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tools := filepath.Join(dir, "tools.yaml")
	if err := os.WriteFile(tools, []byte("agentry-tools:\n  - grep\nagentry-desktop-blender: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := grantDesktopTools(dir, rd.desktop); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(tools) //nolint:gosec // test file
	if got := string(b); !strings.Contains(got, "agentry-desktop-blender:\n  - execute_blender_code\n  - get_scene_info\n  - look") || !strings.Contains(got, "agentry-tools:\n  - grep") {
		t.Fatalf("tools.yaml:\n%s", got)
	}
}

// A persistent agent's segment holds every binding to the policy: the
// first binding's From travels with the segment's link, and a binding made
// at a call, after Agentry Desktop was not answering at the start, is
// refused desktop-not-granted when the policy refuses the person's own
// set-ups.
func TestASegmentsBindingsAreHeldToThePolicy(t *testing.T) {
	d := blenderPlan().Capabilities.Desktop
	d.PersonServers = spec.PersonServersRefused
	answer := func(from string) DesktopBinder {
		return func(context.Context, desktop.BindRequest) (*DesktopLink, error) {
			return &DesktopLink{Token: "t", Servers: map[string][]desktoplink.Tool{"blender": standInBlenderTools},
				From: map[string]string{"blender": from}, Unbind: func(context.Context) error { return nil },
				Call: func(context.Context, desktop.CallRequest) (desktoplink.Answer, error) {
					return desktoplink.TextAnswer("answered", false), nil
				}}, nil
		}
	}
	link, err := PersistentDesktopLink(context.Background(), answer(desktop.FromLibrary), desktop.BindRequest{Run: "S"}, d)
	if err != nil {
		t.Fatal(err)
	}
	if link.From["blender"] != desktop.FromLibrary {
		t.Fatalf("the segment's link lost the first binding's From: %+v", link.From)
	}
	plan := blenderPlan()
	plan.Capabilities.Desktop = d
	if err := checkDesktop(Params{Plan: plan, Desktop: link}); err != nil {
		t.Fatalf("the library's application under a refusing policy: %v", err)
	}

	down := true
	bind := func(ctx context.Context, req desktop.BindRequest) (*DesktopLink, error) {
		if down {
			return nil, desktop.ErrNoService
		}
		return answer(desktop.FromPerson)(ctx, req)
	}
	link, err = PersistentDesktopLink(context.Background(), bind, desktop.BindRequest{Run: "S"}, d)
	if err != nil {
		t.Fatal(err)
	}
	down = false
	_, err = link.Call(context.Background(), desktop.CallRequest{Server: "blender", Tool: "get_scene_info"})
	var r *desktop.Refusal
	if !errors.As(err, &r) || r.Reason != receipt.DesktopNotGranted || r.Server != "blender" || !strings.Contains(r.Message, "desktop.allowCustomServers: false") {
		t.Fatalf("a person's set-up bound at a call: %v", err)
	}
}

// A message to the person names the chosen entry "an application you
// choose", and the application they chose as theirs, while the plan, the
// System Prompt and the receipt keep the agent's words.
func TestTheChosenEntryIsNamedToThePerson(t *testing.T) {
	d := &spec.DesktopCap{
		Servers: []spec.DesktopServerCap{
			{ID: "my-cad", Application: "my-cad"},
			{ID: spec.DesktopChosenID, Application: "an application the person chooses", Chosen: true},
		},
		ComputerUse: spec.ComputerUseApp, PersonServers: spec.PersonServersRefused,
	}
	if err := NoDesktopRefusal(d); !strings.Contains(err.Error(), "this QuickAgent uses my-cad, an application you choose and computer use on your computer") {
		t.Errorf("no Agentry Desktop: %v", err)
	}
	call := func(context.Context, desktop.CallRequest) (desktoplink.Answer, error) {
		return desktoplink.Answer{}, nil
	}
	plan := &spec.Plan{Capabilities: spec.CapSummary{Desktop: d}}

	unbound := &DesktopLink{Token: "t", Call: call, Servers: map[string][]desktoplink.Tool{"my-cad": standInBlenderTools[:1]}}
	if err := checkDesktop(Params{Plan: plan, Desktop: unbound}); err == nil || !strings.Contains(err.Error(), "this QuickAgent uses an application you choose on your computer, and Agentry Desktop did not bind it") {
		t.Errorf("an unbound chosen entry: %v", err)
	}

	refused := &DesktopLink{Token: "t", Call: call,
		Servers: map[string][]desktoplink.Tool{"my-cad": standInBlenderTools[:1], spec.DesktopChosenID: standInBlenderTools[:1]},
		From:    map[string]string{"my-cad": desktop.FromLibrary, spec.DesktopChosenID: desktop.FromPerson},
		Chosen:  &desktop.ChosenApplication{ID: "kdenlive", Name: "Kdenlive"}}
	err := checkDesktop(Params{Plan: plan, Desktop: refused})
	if err == nil || !strings.Contains(err.Error(), "this QuickAgent uses Kdenlive (the application you chose) on your computer") {
		t.Errorf("a refused chosen application: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "the person") {
		t.Errorf("a message to the person speaks of them in the third person: %v", err)
	}
	if got := boundName(refused, d.Servers[1]); got != "Kdenlive (the application the person chose)" {
		t.Errorf("the agent's words for the chosen application: %q", got)
	}
}

// A run refused after Agentry Desktop bound it, by the binding itself or by
// a later step, still ends the binding, once: the policy's refusal of an
// application the person set up, in the preparation and in Run's own check
// before it, and a later step of the preparation that fails, each unbind.
func TestARefusedRunUnbinds(t *testing.T) {
	refused := &spec.Plan{Ref: "acme/cad@1.0.0", Capabilities: spec.CapSummary{Desktop: &spec.DesktopCap{
		Servers: []spec.DesktopServerCap{{ID: "blender", Application: "blender"}}, PersonServers: spec.PersonServersRefused}}}
	s, link := bindStandIn(t, refused, map[string]string{"blender": desktop.FromPerson})
	p, _ := fixture(t)
	p.Plan.Capabilities.Desktop = refused.Capabilities.Desktop
	p.Desktop = link
	td := &teardown{stderr: io.Discard}
	if _, err := prepareRunDirs(p, "cafe0425", td); err == nil || !strings.Contains(err.Error(), "allows only the library's applications") {
		t.Fatalf("the policy's refusal: %v", err)
	}
	td.run()
	td.run()
	if _, _, unbinds := s.sent(); len(unbinds) != 1 {
		t.Fatalf("a refused run unbound %d times, want once", len(unbinds))
	}

	// The run itself refuses before anything is prepared: the binding ends
	// when Run returns.
	s3, link3 := bindStandIn(t, refused, map[string]string{"blender": desktop.FromPerson})
	p3, d3 := fixture(t)
	p3.Plan.Capabilities.Desktop = refused.Capabilities.Desktop
	p3.Desktop = link3
	if _, err := Run(context.Background(), d3, p3); err == nil || !strings.Contains(err.Error(), "allows only the library's applications") {
		t.Fatalf("Run's refusal: %v", err)
	}
	if _, _, unbinds := s3.sent(); len(unbinds) != 1 {
		t.Fatalf("a run Run refused unbound %d times, want once", len(unbinds))
	}

	// A later step fails: the compiled gateway files carry no entry for
	// the application.
	allowed := blenderPlan()
	s2, link2 := bindStandIn(t, allowed, nil)
	p2, _ := fixture(t)
	p2.Plan.Capabilities.Desktop = allowed.Capabilities.Desktop
	p2.Desktop = link2
	td2 := &teardown{stderr: io.Discard}
	if _, err := prepareRunDirs(p2, "cafe0426", td2); err == nil {
		t.Fatal("a run whose gateway files lack the application's entry was prepared")
	}
	td2.run()
	if _, _, unbinds := s2.sent(); len(unbinds) != 1 {
		t.Fatalf("a run that failed after its bind unbound %d times, want once", len(unbinds))
	}
}

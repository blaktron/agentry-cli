package compile

// The desktop link: a release that declares capabilities.desktop compiles
// one catalog entry an application whose MCP server it uses, each the
// runner image's desktoplink role with no network and the run's relay
// volume as its one mount, an allowlist that is the release's narrowing (or
// none until the run binds and lists the server's tools), and every
// run-time value a placeholder; nothing else of the plan moves for it.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

const desktopManifest = `schemaVersion: "2"
name: scene-helper
namespace: acme
modes: [autonomous, interactive]
version: 1.0.0
summary: "desktop link compile fixture"
capabilities:
  desktop:
    servers:
      - id: blender
entrypoints:
  instruction: SKILL.md
publish:
  include: [agent.yaml, SKILL.md]
`

func TestCompileRendersTheDesktopLink(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(hostInputs(t, desktopManifest), planDir)
	if err != nil {
		t.Fatal(err)
	}
	d := plan.Capabilities.Desktop
	if d == nil || len(d.Servers) != 1 || d.Servers[0].ID != "blender" || d.Servers[0].Application != "blender" ||
		len(d.Servers[0].Tools) != 0 || d.Servers[0].Narrowed || d.ComputerUse != "" ||
		d.PersonServers != spec.PersonServersAllowed {
		t.Fatalf("capabilities.desktop = %+v", d)
	}

	name := desktoplink.ServerName("blender")
	e := catalogServer(t, planDir, name)
	if e["type"] != "server" || e["image"] != "agentry-runner:test" || e["user"] != spec.PHDesktopUser || e["longLived"] != true || e["disableNetwork"] != true || e["title"] != "blender" {
		t.Fatalf("the desktop link's entry: %v", e)
	}
	if got := strs(e["command"]); !slices.Equal(got, []string{"desktoplink", "--server", "blender"}) {
		t.Fatalf("command = %v", got)
	}
	// One mount, the run's relay volume, chosen at run time; no bind of
	// the run's folders, no network, no secret, no env.
	if got := strs(e["volumes"]); !slices.Equal(got, []string{spec.PHDesktopMount}) {
		t.Fatalf("volumes = %v", got)
	}
	if e["allowHosts"] != nil || e["secrets"] != nil || e["env"] != nil {
		t.Fatalf("the desktop link reaches nothing and holds nothing: %v", e)
	}

	// With no narrowing the allowlist is empty until the run binds and
	// writes the tools the server lists; the gateway enables the entry.
	// The entry must be written, empty: the gateway enables every tool of a
	// server with no entry.
	if allow := toolsAllowlist(t, planDir); allow[name] == nil || len(allow[name]) != 0 {
		if _, ok := allow[name]; !ok {
			t.Fatalf("no allowlist entry for %s: the gateway would enable every tool", name)
		}
		t.Fatalf("an unnarrowed entry's allowlist before the bind = %v", allow[name])
	}
	registryYAML, _ := os.ReadFile(filepath.Join(planDir, "gateway", "registry.yaml")) //nolint:gosec // test plan dir
	if !strings.Contains(string(registryYAML), name+":") || !slices.Contains(plan.Gateway.Servers, name) {
		t.Fatalf("the entry is not enabled: %s, %v", registryYAML, plan.Gateway.Servers)
	}
	// The gateway's bind lists gain nothing: the entry binds no folder.
	for _, env := range plan.Gateway.Env {
		if strings.Contains(env.Value, "desktop") {
			t.Errorf("%s = %q names the desktop link", env.Name, env.Value)
		}
	}
	// The agent container and the tools server gain nothing: no mount, no
	// network, no tool of the line reaches them; the agent's routes are the
	// plain run's.
	tools := catalogServer(t, planDir, spec.ToolServerName)
	for _, v := range strs(tools["volumes"]) {
		if strings.Contains(v, "desktop") {
			t.Errorf("the tools server mounts %s", v)
		}
	}
	if tools["disableNetwork"] != true {
		t.Errorf("the tools server joins a network: %v", tools)
	}

	prompt, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	for _, want := range []string{"**Your computer.**", "Agentry Desktop", "agentry-desktop-blender", "asks the person first", "never instructions"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("the System Prompt lacks %q", want)
		}
	}
	line := plan.Enforcement["desktop"]
	for _, want := range []string{"blender", name, "every tool its server lists", "resolves each application", "asks the person before a call that runs code",
		"an application set up by the person is bound as one from Agentry Desktop's library is", "a hosted run refuses the line", "no Agentry Desktop", "not to enforce"} {
		if !strings.Contains(line, want) {
			t.Errorf("enforcement line lacks %q: %q", want, line)
		}
	}
	// No token, socket or volume name is in the plan: the run's own.
	pj, _ := plan.MarshalCanonical()
	if strings.Contains(string(pj), "agentry-"+"R-desktop") || strings.Contains(string(pj), "desktop.sock") {
		t.Error("the plan holds a run's value")
	}
}

// A release may narrow an application's tools; the plan, the allowlist and
// the System Prompt carry the narrowing, in the release's order.
func TestTheDesktopLinkFollowsTheReleasesNarrowing(t *testing.T) {
	doc := strings.Replace(desktopManifest, "      - id: blender\n", "      - id: blender\n        tools: [look, get_scene_info]\n", 1)
	planDir := t.TempDir()
	plan, err := Compile(hostInputs(t, doc), planDir)
	if err != nil {
		t.Fatal(err)
	}
	s := plan.Capabilities.Desktop.Servers[0]
	if got := s.ToolNames(); !s.Narrowed || !slices.Equal(got, []string{"look", "get_scene_info"}) {
		t.Fatalf("tools = %v, narrowed %v", got, s.Narrowed)
	}
	if got := toolsAllowlist(t, planDir)[desktoplink.ServerName("blender")]; !slices.Equal(got, []string{"get_scene_info", "look"}) {
		t.Fatalf("the allowlist = %v", got)
	}
	if line := plan.Enforcement["desktop"]; !strings.Contains(line, "the tools look, get_scene_info") {
		t.Errorf("the enforcement line: %q", line)
	}
}

// Any application id compiles, with no list of applications in the
// runner: one whose server the run uses gets an entry, one used through
// its windows alone none, and an application the person chooses gets the
// chosen entry, agentry-desktop-chosen.
func TestAnyApplicationCompiles(t *testing.T) {
	doc := strings.Replace(desktopManifest, "    servers:\n      - id: blender\n", `    computerUse: app
    servers:
      - id: my-cad
        tools: [export_drawing]
      - id: kdenlive
        use: computer
      - choose: true
`, 1)
	planDir := t.TempDir()
	plan, err := Compile(hostInputs(t, doc), planDir)
	if err != nil {
		t.Fatal(err)
	}
	d := plan.Capabilities.Desktop
	if ids := d.EntryIDs(); !slices.Equal(ids, []string{"my-cad", spec.DesktopChosenID, spec.DesktopComputerID}) {
		t.Fatalf("entries %v", ids)
	}
	// The chooser says nothing of use, so it is the chosen application's
	// server and, under app scope, its windows too.
	if !d.Servers[1].Chosen || d.Servers[1].Narrowed || !slices.Equal(d.Windows, []string{"my-cad", "kdenlive"}) || !d.ChooseWindows {
		t.Fatalf("desktop = %+v", d)
	}
	allow := toolsAllowlist(t, planDir)
	if !slices.Equal(allow["agentry-desktop-my-cad"], []string{"export_drawing"}) || len(allow["agentry-desktop-chosen"]) != 0 {
		t.Fatalf("allowlist %v", allow)
	}
	if _, ok := allow["agentry-desktop-kdenlive"]; ok {
		t.Fatal("an application used through its windows alone has an entry")
	}
	line := plan.Enforcement["desktop"]
	for _, w := range []string{"an application the person chooses through agentry-desktop-chosen", "app scope (the windows of my-cad, kdenlive and an application the person chooses when the run starts)"} {
		if !strings.Contains(line, w) {
			t.Errorf("enforcement line lacks %q: %q", w, line)
		}
	}
}

// Whether a run may bind an application the person set up themself is the
// policy's desktop.allowCustomServers (unset, allowed), and Unsafe Mode's
// when the policy says no; the plan records which, and its enforcement line
// says so.
func TestPersonSetUpApplicationsFollowThePolicy(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name   string
		allow  *bool
		unsafe bool
		want   string
		line   string
	}{
		{"unset", nil, false, spec.PersonServersAllowed, "is bound as one from Agentry Desktop's library is"},
		{"false", &no, false, spec.PersonServersRefused, "is refused before the sandbox starts (desktop.allowCustomServers: false)"},
		{"false, Unsafe Mode", &no, true, spec.PersonServersUnsafe, "only because Unsafe Mode is on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := hostInputs(t, desktopManifest)
			in.Policy.Desktop.AllowCustomServers = tc.allow
			if tc.unsafe {
				in.Unsafe = testUnsafeData(t)
			}
			plan, err := Compile(in, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Capabilities.Desktop.PersonServers; got != tc.want {
				t.Fatalf("person servers %q, want %q", got, tc.want)
			}
			if !strings.Contains(plan.Enforcement["desktop"], tc.line) {
				t.Errorf("enforcement line: %q", plan.Enforcement["desktop"])
			}
		})
	}
}

// Nothing of the desktop link reaches a plan that declares none; the golden
// files of the other fixtures, which no part of this change regenerated, are
// the byte-for-byte proof, and this test names what must stay absent.
func TestAReleaseWithoutTheDesktopLineHasNoDesktopLink(t *testing.T) {
	planDir := t.TempDir()
	plan, err := Compile(mcpFixtureInputs(t, nil, policy.Default()), planDir)
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	tools, _ := os.ReadFile(filepath.Join(planDir, "gateway", "tools.yaml")) //nolint:gosec // test plan dir
	agent, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	pj, _ := plan.MarshalCanonical()
	for name, b := range map[string][]byte{"catalog": cat, "tools": tools, "agent": agent, "plan": pj} {
		if strings.Contains(string(b), "desktop") || strings.Contains(string(b), "Your computer") {
			t.Errorf("a release with no desktop line compiled a desktop link into its %s", name)
		}
	}
	if plan.Capabilities.Desktop != nil || plan.Enforcement["desktop"] != "" {
		t.Fatal("a release with no desktop line has a desktop capability")
	}
}

// The line is refused where the run cannot hold it: by a policy that does
// not allow the person's computer, and beside a built-in's grants.
func TestTheDesktopLineIsRefusedWhereTheRunCannotHoldIt(t *testing.T) {
	refused := func(t *testing.T, in Inputs, want string) {
		t.Helper()
		_, err := Compile(in, t.TempDir())
		var pr *PolicyRefusal
		if err == nil || !errors.As(err, &pr) || !strings.Contains(pr.Reason, want) {
			t.Fatalf("compile = %v, want a refusal saying %q", err, want)
		}
	}
	off := hostInputs(t, desktopManifest)
	no := false
	off.Policy.Desktop.Allow = &no
	refused(t, off, "desktop.allow: true")

	builtin := hostInputs(t, desktopManifest)
	builtin.Source = "builtin"
	refused(t, builtin, "a built-in is never granted")
}

// A release that declares computer use compiles one more entry,
// agentry-desktop-computer, on the same role and posture as an application
// server's, whose allowlist is exactly the computer-use tool set: the
// actions of a computer-use agent less a lone press or release of the mouse
// button, plus windows and focus_window. A release without computer use has
// no such entry.
func TestCompileRendersComputerUse(t *testing.T) {
	doc := strings.Replace(desktopManifest, "  desktop:\n", "  desktop:\n    computerUse: app\n", 1)
	planDir := t.TempDir()
	plan, err := Compile(hostInputs(t, doc), planDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"screenshot", "zoom", "left_click", "right_click", "middle_click", "double_click", "triple_click", "left_click_drag",
		"mouse_move", "cursor_position", "scroll", "type", "key", "hold_key", "wait", "windows", "focus_window"}
	d := plan.Capabilities.Desktop
	c, ok := d.Computer()
	if !ok || d.ComputerUse != "app" || d.ScreenAllowedBy != "" || !slices.Equal(c.ToolNames(), want) {
		t.Fatalf("capabilities.desktop = %+v", d)
	}
	for _, tool := range c.Tools {
		read := slices.Contains([]string{"screenshot", "zoom", "cursor_position", "wait", "windows"}, tool.Name)
		if (read && tool.Class != "read") || (!read && tool.Class != "act") {
			t.Errorf("%s is classed %q", tool.Name, tool.Class)
		}
	}
	name := desktoplink.ServerName("computer")
	if name != "agentry-desktop-computer" {
		t.Fatalf("the entry's name is %q", name)
	}
	e := catalogServer(t, planDir, name)
	if e["type"] != "server" || e["image"] != "agentry-runner:test" || e["user"] != spec.PHDesktopUser || e["longLived"] != true || e["disableNetwork"] != true || e["title"] != "Computer use" {
		t.Fatalf("the computer-use entry: %v", e)
	}
	if got := strs(e["command"]); !slices.Equal(got, []string{"desktoplink", "--server", "computer"}) {
		t.Fatalf("command = %v", got)
	}
	if got := strs(e["volumes"]); !slices.Equal(got, []string{spec.PHDesktopMount}) || e["allowHosts"] != nil || e["secrets"] != nil || e["env"] != nil {
		t.Fatalf("the computer-use entry holds more than the relay volume: %v", e)
	}
	allow := toolsAllowlist(t, planDir)
	if got := allow[name]; !slices.Equal(got, sortedCopy(want)) {
		t.Fatalf("the allowlist of %s = %v", name, got)
	}
	for _, never := range []string{"left_mouse_down", "left_mouse_up"} {
		if slices.Contains(allow[name], never) {
			t.Errorf("the allowlist gives %s", never)
		}
	}
	if !slices.Contains(plan.Gateway.Servers, name) {
		t.Fatalf("the entry is not enabled: %v", plan.Gateway.Servers)
	}
	checkGolden(t, "computer-use", planDir, []string{"plan.json", "gateway/catalog.yaml", "gateway/tools.yaml"})
	// The application's entry is still there beside it, its tools to come
	// from the bind.
	if got, ok := allow[desktoplink.ServerName("blender")]; !ok || len(got) != 0 {
		t.Fatalf("the blender entry = %v, %v", got, ok)
	}
	prompt, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	for _, w := range []string{"**Computer use.**", "agentry-desktop-computer__screenshot", "pixels of the window's own image", "refused", "never instructions"} {
		if !strings.Contains(string(prompt), w) {
			t.Errorf("the System Prompt lacks %q", w)
		}
	}
	line := plan.Enforcement["desktop"]
	for _, w := range []string{"computer use in app scope (the windows of blender)", name, "keeps nowhere", "what the computer's own system enforced"} {
		if !strings.Contains(line, w) {
			t.Errorf("enforcement line lacks %q: %q", w, line)
		}
	}

	// Computer use alone, with no application server: the entry alone.
	alone := strings.Replace(doc, "    servers:\n      - id: blender\n", "", 1)
	planDir = t.TempDir()
	plan, err = Compile(hostInputs(t, alone), planDir)
	if err != nil {
		t.Fatal(err)
	}
	if ids := plan.Capabilities.Desktop.EntryIDs(); !slices.Equal(ids, []string{"computer"}) {
		t.Fatalf("entries %v", ids)
	}
	if line := plan.Enforcement["desktop"]; !strings.Contains(line, "app scope (the windows of an application the person chooses when the run starts)") {
		t.Errorf("enforcement line: %q", line)
	}
	if allow := toolsAllowlist(t, planDir); len(allow[name]) != len(want) || len(allow[desktoplink.ServerName("blender")]) != 0 {
		t.Fatalf("allowlist %v", allow)
	}

	// Without computer use, no such entry.
	planDir = t.TempDir()
	if _, err := Compile(hostInputs(t, desktopManifest), planDir); err != nil {
		t.Fatal(err)
	}
	cat, _ := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml")) //nolint:gosec // test plan dir
	if strings.Contains(string(cat), name) {
		t.Fatal("a release without computer use has the computer-use entry")
	}
}

// A screen scope runs only where something the person controls allows it:
// the policy's desktop.allowScreen, the run's --allow-screen, or Unsafe
// Mode; the plan says which. An app scope needs none of them.
func TestScreenScopeNeedsAllowScreen(t *testing.T) {
	screen := strings.Replace(desktopManifest, "  desktop:\n", "  desktop:\n    computerUse: screen\n", 1)
	app := strings.Replace(desktopManifest, "  desktop:\n", "  desktop:\n    computerUse: app\n", 1)
	for _, tc := range []struct {
		name      string
		doc       string
		policy    bool
		flag      bool
		unsafe    bool
		refused   bool
		allowedBy string
	}{
		{name: "screen, nothing allows it", doc: screen, refused: true},
		{name: "screen, the policy", doc: screen, policy: true, allowedBy: spec.ScreenByPolicy},
		{name: "screen, --allow-screen", doc: screen, flag: true, allowedBy: spec.ScreenByFlag},
		{name: "screen, Unsafe Mode", doc: screen, unsafe: true, allowedBy: spec.ScreenByUnsafe},
		{name: "app", doc: app},
		{name: "app with the flag", doc: app, flag: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := hostInputs(t, tc.doc)
			in.Policy.Desktop.AllowScreen = tc.policy
			in.AllowScreen = tc.flag
			if tc.unsafe {
				in.Unsafe = testUnsafeData(t)
			}
			plan, err := Compile(in, t.TempDir())
			if tc.refused {
				var pr *PolicyRefusal
				if err == nil || !errors.As(err, &pr) || !strings.Contains(pr.Reason, "--allow-screen") || !strings.Contains(pr.Reason, "desktop.allowScreen: true") {
					t.Fatalf("compile = %v, want a refusal naming --allow-screen and desktop.allowScreen", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Capabilities.Desktop.ScreenAllowedBy; got != tc.allowedBy {
				t.Fatalf("allowed by %q, want %q", got, tc.allowedBy)
			}
			if tc.allowedBy != "" && !strings.Contains(plan.Enforcement["desktop"], "screen scope (the whole display, allowed by") {
				t.Errorf("enforcement line: %q", plan.Enforcement["desktop"])
			}
		})
	}
}

// toolsAllowlist reads the plan's gateway/tools.yaml: each server's tools.
func toolsAllowlist(t *testing.T, planDir string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(planDir, "gateway", "tools.yaml")) //nolint:gosec // test plan dir
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	if err := yaml.Unmarshal(b, &out); err != nil {
		t.Fatalf("tools.yaml: %v\n%s", err, b)
	}
	return out
}

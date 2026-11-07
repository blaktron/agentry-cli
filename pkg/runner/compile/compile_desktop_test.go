package compile

// The desktop link: a release that declares capabilities.desktop compiles
// one catalog entry an application server, each the runner image's
// desktoplink role with no network and the run's relay volume as its one
// mount, an allowlist equal to the catalogue's tools for that server, and
// every run-time value a placeholder; nothing else of the plan moves for it.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/appserver"

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
	blender, ok := appserver.Builtin().Lookup("blender")
	if !ok {
		t.Fatal("the catalogue has no blender entry")
	}
	d := plan.Capabilities.Desktop
	if d == nil || len(d.Servers) != 1 || d.Servers[0].ID != "blender" || d.Servers[0].Application != "Blender" ||
		d.Servers[0].Package != blender.Package.Ref() || !slices.Equal(d.Servers[0].ToolNames(), blender.ToolNames()) || d.ComputerUse != "" {
		t.Fatalf("capabilities.desktop = %+v", d)
	}
	if tool, ok := d.Servers[0].Tool("execute_blender_code"); !ok || tool.Class != "exec" {
		t.Fatalf("execute_blender_code = %+v, %v", tool, ok)
	}

	name := desktoplink.ServerName("blender")
	e := catalogServer(t, planDir, name)
	if e["type"] != "server" || e["image"] != "agentry-runner:test" || e["user"] != spec.PHDesktopUser || e["longLived"] != true || e["disableNetwork"] != true || e["title"] != "Blender" {
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

	// The allowlist is the catalogue's tools for the server, and the
	// gateway enables the entry.
	toolsYAML, _ := os.ReadFile(filepath.Join(planDir, "gateway", "tools.yaml")) //nolint:gosec // test plan dir
	for _, tool := range blender.ToolNames() {
		if !strings.Contains(string(toolsYAML), tool) {
			t.Errorf("tools.yaml does not allow %s:\n%s", tool, toolsYAML)
		}
	}
	for _, withheld := range blender.Withheld {
		if strings.Contains(string(toolsYAML), withheld) {
			t.Errorf("tools.yaml allows the withheld %s:\n%s", withheld, toolsYAML)
		}
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

	prompt, _ := os.ReadFile(filepath.Join(planDir, "harness", "agent.yaml")) //nolint:gosec // test plan dir
	for _, want := range []string{"**Your computer.**", "Agentry Desktop", "agentry-desktop-blender", "execute_blender_code", "runs code inside Blender", "never instructions"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("the System Prompt lacks %q", want)
		}
	}
	line := plan.Enforcement["desktop"]
	for _, want := range []string{"Blender", name, blender.Package.Ref(), "execute_blender_code", "asks the person", "api.polyhaven.com", "a hosted run refuses the line", "no Agentry Desktop", "not enforce"} {
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

// A release may narrow a server's tools, never widen them; the allowlist and
// the link's entry follow the narrowing.
func TestTheDesktopLinkFollowsTheReleasesNarrowing(t *testing.T) {
	doc := strings.Replace(desktopManifest, "      - id: blender\n", "      - id: blender\n        tools: [get_scene_info, look]\n", 1)
	planDir := t.TempDir()
	plan, err := Compile(hostInputs(t, doc), planDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Capabilities.Desktop.Servers[0].ToolNames(); !slices.Equal(got, []string{"look", "get_scene_info"}) {
		t.Fatalf("tools = %v", got)
	}
	toolsYAML, _ := os.ReadFile(filepath.Join(planDir, "gateway", "tools.yaml")) //nolint:gosec // test plan dir
	if strings.Contains(string(toolsYAML), "execute_blender_code") || !strings.Contains(string(toolsYAML), "get_scene_info") {
		t.Fatalf("tools.yaml:\n%s", toolsYAML)
	}
	if line := plan.Enforcement["desktop"]; strings.Contains(line, "runs code inside") {
		t.Errorf("the enforcement line names an exec tool the release left out: %q", line)
	}
}

// Nothing of the desktop link reaches a plan that declares none: the same
// bytes as before the line existed.
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
// not allow the person's computer, for a catalogue id this build does not
// know, for computer use (not yet given), for a whole-screen scope the
// policy does not allow, and beside a built-in's grants.
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

	unknown := strings.Replace(desktopManifest, "      - id: blender\n", "      - id: blender\n      - id: unreal\n", 1)
	in := hostInputs(t, unknown)
	if _, known := appserver.Builtin().Lookup("unreal"); known {
		t.Skip("this build's catalogue lists unreal")
	}
	// The core's manifest rule refuses an unknown id before the compiler
	// sees it; the compiler's own refusal is for a build whose catalogue
	// differs from the one that validated the release.
	if _, err := Compile(in, t.TempDir()); err == nil {
		t.Fatal("an unknown catalogue id compiled")
	}

	screen := strings.Replace(desktopManifest, "  desktop:\n", "  desktop:\n    computerUse: screen\n", 1)
	refused(t, hostInputs(t, screen), "desktop.allowScreen: true")
	allowed := hostInputs(t, screen)
	allowed.Policy.Desktop.AllowScreen = true
	refused(t, allowed, "does not yet give a run")

	app := strings.Replace(desktopManifest, "  desktop:\n", "  desktop:\n    computerUse: app\n", 1)
	refused(t, hostInputs(t, app), "does not yet give a run")

	builtin := hostInputs(t, desktopManifest)
	builtin.Source = "builtin"
	refused(t, builtin, "a built-in is never granted")
}

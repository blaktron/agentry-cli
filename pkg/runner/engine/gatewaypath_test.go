package engine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func TestGatewayHostPath(t *testing.T) {
	cases := []struct{ in, goos, want string }{
		{`C:\Users\blakt\.agentry\runner\runs\ae8a24dd\cfg`, "windows", "/c/Users/blakt/.agentry/runner/runs/ae8a24dd/cfg"},
		{`d:\work\out\`, "windows", "/d/work/out"},
		{`C:/Users/u/cfg`, "windows", "/c/Users/u/cfg"},
		{`C:\`, "windows", "/c"},
		// Not a drive path: nothing to spell differently.
		{`\\server\share\x`, "windows", `\\server\share\x`},
		{"", "windows", ""},
		// Linux and macOS paths are the gateway's already.
		{"/home/u/.agentry/runs/r/cfg", "linux", "/home/u/.agentry/runs/r/cfg"},
		{"/Users/u/.agentry/runs/r/cfg", "darwin", "/Users/u/.agentry/runs/r/cfg"},
	}
	for _, c := range cases {
		if got := gatewayHostPath(c.in, c.goos); got != c.want {
			t.Errorf("gatewayHostPath(%q, %s) = %q, want %q", c.in, c.goos, got, c.want)
		}
	}
}

// On a Windows host every volume the gateway launches the tools server
// with must pass the gateway's own bind check: its allow-lists split on
// ":" (filepath.SplitList in a Linux container), and each host source must
// sit under a root, writable ones by exact entry. A C:\ root split at the
// drive colon failed every run's tools server (agentry-cli#414). The run's
// own wiring renders the config and the env, so a call site that went
// back to the native paths fails here too.
func TestGatewaySeesWindowsPathsItCanCheck(t *testing.T) {
	p, _ := fixture(t)
	p.Knowledge = []AttachedPack{AttachPack(makePack(t, "voice"))}
	setGatewayOS(t, "windows")

	run := `C:\Users\blakt\.agentry\runner\agents\agentry\drafter\1.0.0-1ab1787b6504\runs\ae8a24dd`
	rd := &runDirs{
		root:       run,
		workspace:  run + `\workspace`,
		inputs:     run + `\inputs`,
		knowledge:  run + `\knowledge`,
		cfg:        t.TempDir(), // written here; the gateway sees its mount, not this path
		outDir:     `C:\Users\blakt\Downloads\agentry-out\agentry-drafter-ae8a24dd`,
		serversDir: `C:\Users\blakt\.agentry\runner\servers\sig`,
	}
	if err := renderGatewayConfig(p, rd); err != nil {
		t.Fatalf("render the gateway config: %v", err)
	}
	env, err := rd.gatewayEnv(p.Plan, "tok")
	if err != nil {
		t.Fatal(err)
	}
	roots := strings.Split(env["MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS"], ":")
	writable := strings.Split(env["MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS"], ":")
	for _, r := range append(append([]string{}, roots...), writable...) {
		if !strings.HasPrefix(r, "/c/Users/blakt/") && !strings.HasPrefix(r, "/tmp/") {
			t.Errorf("gateway root %q is not a /c/ path (roots %q)", r, roots)
		}
	}

	raw, err := os.ReadFile(filepath.Join(rd.cfg, "gateway", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Registry map[string]struct {
			Volumes []string `yaml:"volumes"`
		} `yaml:"registry"`
	}
	if err := yaml.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	vols := catalog.Registry[spec.ToolServerName].Volumes
	if len(vols) == 0 {
		t.Fatalf("the tools server has no volumes in the catalog: %s", raw)
	}
	for _, v := range vols {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			t.Errorf("volume %q does not split into source:target[:mode]", v)
			continue
		}
		src := parts[0]
		if strings.Contains(src, `\`) {
			t.Errorf("volume %q: source keeps a Windows separator", v)
		}
		if len(parts) == 3 && parts[2] == "rw" {
			if !slices.Contains(writable, src) {
				t.Errorf("writable volume %q is not an exact writable entry %q", v, writable)
			}
			continue
		}
		if !underAny(src, roots) {
			t.Errorf("volume %q: source %q is outside the roots %q", v, src, roots)
		}
	}

	// The CLI's own docker calls go through the Windows named pipe and
	// keep the native path, as the gateway's config mount does today.
	if got := rd.replacer().Replace(spec.PHCfg + "/gateway"); got != rd.cfg+"/gateway" {
		t.Errorf("native replacer = %q, want the host path", got)
	}
	if got := rd.replacer().Replace(spec.PHWorkspace); got != run+`\workspace` {
		t.Errorf("native replacer = %q, want the Windows path", got)
	}
}

// Elsewhere the gateway reads exactly what the rest of the run does.
func TestGatewayReplacerIsTheReplacerOffWindows(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		setGatewayOS(t, goos)
		rd := &runDirs{
			root: "/r/runs/x", workspace: "/r/runs/x/workspace", inputs: "/r/runs/x/inputs",
			knowledge: "/r/runs/x/knowledge", cfg: "/r/runs/x/cfg", secretsPath: "/r/runs/x/cfg/gateway/secrets.env",
			outDir: "/o/out", serversDir: "/s/sig", session: "vol",
		}
		all := strings.Join([]string{spec.PHWorkspace, spec.PHOut, spec.PHCfg, spec.PHSecrets,
			spec.PHInputs, spec.PHKnowledge, spec.PHSession, spec.PHServers, spec.PHRunID}, ":")
		if g, n := rd.gatewayReplacer().Replace(all), rd.replacer().Replace(all); g != n {
			t.Errorf("%s: gateway replacer %q, replacer %q", goos, g, n)
		}
	}
}

func setGatewayOS(t *testing.T, goos string) {
	t.Helper()
	old := gatewayOS
	gatewayOS = goos
	t.Cleanup(func() { gatewayOS = old })
}

func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if r != "" && (p == r || strings.HasPrefix(p, r+"/")) {
			return true
		}
	}
	return false
}

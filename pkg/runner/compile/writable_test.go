package compile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestEveryWritableBindHasAnExactEntry: the gateway starts a server with a
// writable host bind only when that exact host path is in
// MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS, and the run's docker filter
// refuses any bind whose source sits inside a writable root without being
// that root. A paths write grant bound workspace/<p> rw while the list
// named only the whole workspace, so the tools server never started (exit
// 13). Every filesystem shape a manifest can take is held to both rules
// here.
func TestEveryWritableBindHasAnExactEntry(t *testing.T) {
	base := "schemaVersion: \"2\"\nname: writable\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: \"w\"\nentrypoints:\n  instruction: SKILL.md\npublish:\n  include: [agent.yaml, SKILL.md]\n"
	shapes := map[string]string{
		"no filesystem":               "",
		"whole read":                  "capabilities:\n  filesystem:\n    read: workspace\n",
		"whole read, whole write":     "capabilities:\n  filesystem:\n    read: workspace\n    write: workspace\n",
		"whole read, write paths":     "capabilities:\n  filesystem:\n    read: workspace\n    write: { paths: [out] }\n",
		"whole read, two write paths": "capabilities:\n  filesystem:\n    read: workspace\n    write: { paths: [reports, notes/summary.md] }\n",
		"read paths, write paths":     "capabilities:\n  filesystem:\n    read: { paths: [docs, \"data/in.csv\"] }\n    write: { paths: [out] }\n",
		"write paths alone":           "capabilities:\n  filesystem:\n    write: { paths: [out] }\n",
		"read and write one path":     "capabilities:\n  filesystem:\n    read: { paths: [out] }\n    write: { paths: [out] }\n",
		"nested write paths":          "capabilities:\n  filesystem:\n    write: { paths: [out, out/sub] }\n",
		"read inside a write path":    "capabilities:\n  filesystem:\n    read: { paths: [out/data, docs] }\n    write: { paths: [out] }\n",
		"write inside a read path":    "capabilities:\n  filesystem:\n    read: { paths: [data] }\n    write: { paths: [data/out] }\n",
		"whole read, nested writes":   "capabilities:\n  filesystem:\n    read: workspace\n    write: { paths: [out, out/sub] }\n",
	}
	for name, caps := range shapes {
		t.Run(name, func(t *testing.T) {
			m := parseManifest(t, base+caps)
			plan, planDir := compileFixture(t, m, workspaceFor(t, m, nil))
			var writable []string
			for _, e := range plan.Gateway.Env {
				if e.Name == "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS" {
					writable = strings.Split(e.Value, ":")
				}
			}
			if len(writable) == 0 {
				t.Fatal("the gateway has no writable bind list")
			}
			raw, err := os.ReadFile(filepath.Join(planDir, "gateway", "catalog.yaml"))
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
				t.Fatalf("the tools server has no volumes: %s", raw)
			}
			var rw []string
			for _, v := range vols {
				parts := strings.Split(v, ":")
				src := parts[0]
				if len(parts) == 3 && parts[2] == "rw" {
					rw = append(rw, src)
					if !slices.Contains(writable, src) {
						t.Errorf("writable bind %q has no exact entry in %q", v, writable)
					}
				}
				for _, root := range writable {
					if strings.HasPrefix(src, root+"/") {
						t.Errorf("bind %q sits inside the writable root %q, which the docker filter refuses", v, root)
					}
				}
			}
			// And no entry the tools server does not bind writable: the
			// list is the run's write grant, not a wider one. Entries under
			// cfg are the package servers' token folders, which these
			// fixtures do not declare but the rule exempts.
			for _, root := range writable {
				if !slices.Contains(rw, root) && !strings.HasPrefix(root, spec.PHCfg+"/") {
					t.Errorf("writable entry %q is no writable bind of the tools server (%q)", root, rw)
				}
			}
		})
	}
}

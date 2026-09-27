package installed

// The capability diff (§11 install row). The whole value of the install
// manifest is what it makes the client *ask* about, so these tests are about
// which changes count as widening.

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/configdir"
)

// TestSnapshotFlattensTheContract pins Snapshot: the manifest's union
// shapes become the flat, comparable form the capability diff (and the
// install/run consent gate) reads.
func TestSnapshotFlattensTheContract(t *testing.T) {
	m, err := manifest.Parse([]byte(`schemaVersion: "2"
name: snap
namespace: demo
modes: [autonomous]
version: 1.0.0
summary: "snapshot fixture"
capabilities:
  network:
    egress: [b.example.com, a.example.com]
  filesystem:
    read: workspace
    write: { paths: [out] }
  exec: { commands: [jq] }
  secrets: [TOKEN]
  dataAccess: [tickets]
publish:
  include: [agent.yaml]
`))
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot(m.Capabilities)
	if len(snap.EgressHosts) != 2 {
		t.Errorf("egress hosts = %v", snap.EgressHosts)
	}
	if snap.FSRead != "workspace" || snap.FSWrite != "paths" {
		t.Errorf("fs = %q/%q", snap.FSRead, snap.FSWrite)
	}
	if len(snap.FSWritePaths) != 1 || snap.FSWritePaths[0] != "out" {
		t.Errorf("write paths = %v", snap.FSWritePaths)
	}
	if len(snap.ExecCommands) != 1 || snap.ExecCommands[0] != "jq" {
		t.Errorf("exec commands = %v", snap.ExecCommands)
	}
	if len(snap.Secrets) != 1 || snap.Secrets[0] != "TOKEN" {
		t.Errorf("secrets = %v", snap.Secrets)
	}
	// A snapshot of the same contract must equal itself: the diff gate
	// compares snapshots, so any instability here would re-prompt forever.
	if !DiffCapabilities(snap, Snapshot(m.Capabilities)).Empty() {
		t.Error("snapshotting the same contract twice produced a diff")
	}
}

func TestDiffCapabilitiesWidening(t *testing.T) {
	tests := []struct {
		name     string
		old, new CapSnapshot
		widened  bool
		contains string
	}{
		{
			name:     "new egress host",
			old:      CapSnapshot{EgressNone: true},
			new:      CapSnapshot{EgressHosts: []string{"evil.example"}},
			widened:  true,
			contains: "adds network egress to evil.example",
		},
		{
			name:     "wildcard egress is called out as unrestricted",
			old:      CapSnapshot{EgressHosts: []string{"api.example"}},
			new:      CapSnapshot{EgressHosts: []string{"api.example", "*"}},
			widened:  true,
			contains: "UNRESTRICTED",
		},
		{
			name:     "exec none to commands",
			old:      CapSnapshot{},
			new:      CapSnapshot{ExecCommands: []string{"git"}},
			widened:  true,
			contains: "adds subprocess execution: git (previously none)",
		},
		{
			name:     "filesystem write escalation",
			old:      CapSnapshot{FSWrite: "none"},
			new:      CapSnapshot{FSWrite: "workspace"},
			widened:  true,
			contains: "widens filesystem write from none to workspace",
		},
		{
			name:     "a new explicit write path",
			old:      CapSnapshot{FSWrite: "paths", FSWritePaths: []string{"out"}},
			new:      CapSnapshot{FSWrite: "paths", FSWritePaths: []string{"out", "/etc"}},
			widened:  true,
			contains: "adds filesystem write path: /etc",
		},
		{
			name:     "a new secret",
			old:      CapSnapshot{Secrets: []string{"API_KEY"}},
			new:      CapSnapshot{Secrets: []string{"API_KEY", "AWS_SECRET_ACCESS_KEY"}},
			widened:  true,
			contains: "asks for a new secret: AWS_SECRET_ACCESS_KEY",
		},
		{
			name:     "new data access",
			old:      CapSnapshot{},
			new:      CapSnapshot{DataAccess: []string{"source-code"}},
			widened:  true,
			contains: "adds data access: source-code",
		},
		{
			name:     "dropping an egress host is a narrowing",
			old:      CapSnapshot{EgressHosts: []string{"a.example", "b.example"}},
			new:      CapSnapshot{EgressHosts: []string{"a.example"}},
			widened:  false,
			contains: "drops network egress to b.example",
		},
		{
			name:     "narrowing write access",
			old:      CapSnapshot{FSWrite: "workspace"},
			new:      CapSnapshot{FSWrite: "none"},
			widened:  false,
			contains: "narrows filesystem write from workspace to none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DiffCapabilities(tt.old, tt.new)
			if d.HasWidened() != tt.widened {
				t.Fatalf("widened = %v, want %v (widened=%v narrowed=%v)",
					d.HasWidened(), tt.widened, d.Widened, d.Narrowed)
			}
			all := strings.Join(append(append([]string{}, d.Widened...), d.Narrowed...), " | ")
			if !strings.Contains(all, tt.contains) {
				t.Errorf("diff %q does not contain %q", all, tt.contains)
			}
		})
	}
}

// TestDiffCapabilitiesIdentical: an unchanged contract must produce nothing,
// or every upgrade re-asks a question the user already answered.
func TestDiffCapabilitiesIdentical(t *testing.T) {
	c := CapSnapshot{
		EgressHosts: []string{"api.example"}, ExecCommands: []string{"git"},
		FSRead: "workspace", FSWrite: "paths", FSWritePaths: []string{"out"},
		Secrets: []string{"API_KEY"}, DataAccess: []string{"source-code"},
	}
	if d := DiffCapabilities(c, c); !d.Empty() {
		t.Fatalf("identical contracts diffed: %+v", d)
	}
}

// TestUndeclaredVsExplicitNone guards the ranking: a manifest that stops
// declaring anything is a step DOWN from explicit "none", but it is not a
// step up into access.
func TestUndeclaredVsExplicitNone(t *testing.T) {
	toUndeclared := DiffCapabilities(CapSnapshot{FSRead: "none"}, CapSnapshot{FSRead: ""})
	if !toUndeclared.HasWidened() {
		t.Error("none -> undeclared should be reported as a widening (we know less than before)")
	}
	toWorkspace := DiffCapabilities(CapSnapshot{FSRead: ""}, CapSnapshot{FSRead: "workspace"})
	if !toWorkspace.HasWidened() {
		t.Error("undeclared -> workspace must be a widening")
	}
	toNone := DiffCapabilities(CapSnapshot{FSRead: ""}, CapSnapshot{FSRead: "none"})
	if toNone.HasWidened() {
		t.Error("undeclared -> none is a narrowing, not a widening")
	}
}

// TestInstallStateRoundTrip: the record must survive YAML, including the
// union-shaped capability fields that a naive marshal of manifest.Capabilities
// would silently drop.
func TestInstallStateRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv(configdir.EnvVar, filepath.Join(home, "config"))

	key := Key{
		Registry: "https://reg.example/", Namespace: "acme", Name: "helper",
		Target: "claude-skill", Dir: "/tmp/skills/helper",
	}
	caps := CapSnapshot{
		EgressHosts: []string{"api.example"}, FSWrite: "paths",
		FSWritePaths: []string{"out"}, Secrets: []string{"API_KEY"},
	}
	var st State
	st.Upsert(key, "1.0.0", caps, true)
	if err := Save(st); err != nil {
		t.Fatal(err)
	}

	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := back.Find(key)
	if rec == nil {
		t.Fatalf("record not found after reload: %+v", back.Records)
	}
	if rec.Version != "1.0.0" {
		t.Errorf("version = %q", rec.Version)
	}
	if !rec.Managed {
		t.Error("managed flag did not survive the round trip")
	}
	if d := DiffCapabilities(rec.Capabilities, caps); !d.Empty() {
		t.Errorf("capabilities did not survive the round trip: %+v", d)
	}

	// A second record for the SAME agent at a different directory is separate:
	// two installations have independent capability histories.
	other := key
	other.Dir = "/other/place"
	back.Upsert(other, "2.0.0", CapSnapshot{}, false)
	if len(back.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(back.Records))
	}
	if back.Find(key).Version != "1.0.0" {
		t.Error("recording a second directory overwrote the first")
	}

	// Remove deletes exactly the keyed record.
	if !back.Remove(other) {
		t.Error("Remove did not find the second record")
	}
	if back.Remove(other) {
		t.Error("Remove found an already-removed record")
	}
	if len(back.Records) != 1 || back.Find(key) == nil {
		t.Errorf("Remove disturbed the wrong record: %+v", back.Records)
	}
}

// TestSnapshotAndDiffOfServerEnv (agentry-cli#99): a declared server's plain
// env is part of the installed contract. A new or changed value is a change
// to the server and re-asks for consent; a dropped one narrows; the same
// env twice is no diff, and a record written before env existed compares
// as "no env" rather than as corrupt.
func TestSnapshotAndDiffOfServerEnv(t *testing.T) {
	m, err := manifest.Parse([]byte(`schemaVersion: "2"
name: snap
namespace: demo
modes: [autonomous]
version: 1.0.0
summary: "env snapshot fixture"
capabilities:
  mcpServers:
    - npm: github-mcp-server-js
      version: 0.1.7
      secrets: [GITHUB_TOKEN]
      env:
        LOGS_DIR: /tmp/logs
        GITHUB_PERMISSION: read-only
publish:
  include: [agent.yaml]
`))
	if err != nil {
		t.Fatal(err)
	}
	snap := Snapshot(m.Capabilities)
	got := snap.MCPServerEnv["npm:github-mcp-server-js@0.1.7"]
	if len(got) != 2 || got["GITHUB_PERMISSION"] != "read-only" || got["LOGS_DIR"] != "/tmp/logs" {
		t.Fatalf("snapshot env = %v", snap.MCPServerEnv)
	}
	if !DiffCapabilities(snap, Snapshot(m.Capabilities)).Empty() {
		t.Fatal("snapshotting the same env twice produced a diff")
	}

	fresh := DiffCapabilities(CapSnapshot{}, snap)
	if !strings.Contains(strings.Join(fresh.Widened, "|"), "adds MCP server: npm:github-mcp-server-js@0.1.7 (env GITHUB_PERMISSION=read-only, LOGS_DIR=/tmp/logs)") {
		t.Fatalf("a new server's env is not part of its consent line: %+v", fresh)
	}

	old := snap
	old.MCPServerEnv = nil // a record from before env existed
	d := DiffCapabilities(old, snap)
	if !d.HasWidened() || !strings.Contains(strings.Join(d.Widened, "|"), "changes env of MCP server npm:github-mcp-server-js@0.1.7: GITHUB_PERMISSION=read-only") {
		t.Fatalf("new env did not widen: %+v", d)
	}

	next := Snapshot(m.Capabilities)
	next.MCPServerEnv["npm:github-mcp-server-js@0.1.7"] = map[string]string{"GITHUB_PERMISSION": "read-write"}
	d = DiffCapabilities(snap, next)
	if !d.HasWidened() || !strings.Contains(strings.Join(d.Widened, "|"), "GITHUB_PERMISSION=read-write (was read-only)") {
		t.Fatalf("changed value did not widen with the old value named: %+v", d)
	}
	if !strings.Contains(strings.Join(d.Narrowed, "|"), "drops env LOGS_DIR of MCP server npm:github-mcp-server-js@0.1.7") {
		t.Fatalf("dropped key did not narrow: %+v", d)
	}
}

// TestCapabilitiesInvertsSnapshot pins the one piece of new logic the shared
// rule needs (agentry-cli#201): the diff is the core's
// manifest.DiffCapabilities, over manifest.Capabilities, so a stored
// snapshot is turned back into that type first. Snapshotting the result
// must give back the snapshot exactly — every field, a scoped npm server
// whose name carries its own "@", a PyPI server, and a server's env —
// or an installed record would read as a changed contract and re-ask.
func TestCapabilitiesInvertsSnapshot(t *testing.T) {
	snap := CapSnapshot{
		EgressHosts:  []string{"a.example.com", "*"},
		FSRead:       "paths",
		FSReadPaths:  []string{"docs", "src"},
		FSWrite:      "workspace",
		ExecCommands: []string{"git", "jq"},
		Secrets:      []string{"TOKEN"},
		DataAccess:   []string{"code", "tickets"},
		MCPServers:   []string{"npm:@scope/server@1.2.3", "pypi:mcp-thing@0.4.0"},
		MCPServerEnv: map[string]map[string]string{"npm:@scope/server@1.2.3": {"MODE": "ro"}},
	}
	if got := Snapshot(snap.capabilities()); !reflect.DeepEqual(got, snap) {
		t.Errorf("Snapshot(snap.capabilities()) =\n  %+v\nwant\n  %+v", got, snap)
	}

	none := CapSnapshot{EgressNone: true, FSRead: "none", FSWrite: "none"}
	if got := Snapshot(none.capabilities()); !reflect.DeepEqual(got, none) {
		t.Errorf("explicit none: got %+v, want %+v", got, none)
	}
	if got := Snapshot(CapSnapshot{}.capabilities()); !reflect.DeepEqual(got, CapSnapshot{}) {
		t.Errorf("an empty snapshot came back as %+v", got)
	}
}

// TestMalformedServerIDStillReasks: an id Snapshot never writes — a
// hand-edited installed.yaml — must not parse into the next release's
// key and hide a new server. It fails safe: the recorded server reads as
// one no release declares, so the release's server widens and re-asks,
// as the raw-string comparison it replaced did.
func TestMalformedServerIDStillReasks(t *testing.T) {
	next := Snapshot(manifest.Capabilities{MCPServers: []manifest.MCPServer{
		{PyPI: "foo", Version: "1"},
		{NPM: "@scope/x", Version: "2.0.0"},
	}})
	for _, recorded := range []string{"foo@1", "npm:@scope/x", "pypi:", "npm:"} {
		old := CapSnapshot{MCPServers: []string{recorded}}
		if d := DiffCapabilities(old, next); !d.HasWidened() {
			t.Errorf("recorded %q: the release's servers did not widen: %+v", recorded, d)
		}
	}
}

// TestArchiveWideningsHoldTheArchiveToTheConsentedContract
// (agentry-cli#252): what an archive's own agent.yaml declares beyond the
// resolve manifest the person consented to — the core rule's widenings,
// plus a same-pin server's own egress, secrets and tools, which that rule
// does not compare (agentry#527). Matching and narrowing report nothing.
func TestArchiveWideningsHoldTheArchiveToTheConsentedContract(t *testing.T) {
	caps := func(t *testing.T, y string) manifest.Capabilities {
		t.Helper()
		m, err := manifest.Parse([]byte("schemaVersion: \"2\"\nname: w\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: s\n" + y))
		if err != nil {
			t.Fatal(err)
		}
		return m.Capabilities
	}
	osv := func(extra string) string {
		return "capabilities:\n  mcpServers:\n    - npm: \"@acme/osv-mcp\"\n      version: 1.2.3\n" + extra
	}
	for _, tc := range []struct {
		name, consented, archive, want string
	}{
		{"egress added", "", "capabilities:\n  network:\n    egress: [evil.example]\n", "adds network egress to evil.example"},
		{"exec added", "", "capabilities:\n  exec:\n    commands: [curl]\n", "curl"},
		{"server egress", osv(""), osv("      egress: [evil.example]\n"), "adds network egress to evil.example for MCP server npm:@acme/osv-mcp@1.2.3"},
		{"server secret", osv(""), osv("      secrets: [OSV_TOKEN]\n"), "asks for a new secret OSV_TOKEN for MCP server npm:@acme/osv-mcp@1.2.3"},
		{"server tools lifted", osv("      tools: [query]\n"), osv(""), "grants MCP server npm:@acme/osv-mcp@1.2.3 its full tool set"},
		{"server tool added", osv("      tools: [query]\n"), osv("      tools: [query, delete]\n"), "adds tool delete to MCP server npm:@acme/osv-mcp@1.2.3"},
		{"matches", osv("      egress: [api.osv.dev]\n      tools: [query]\n"), osv("      egress: [api.osv.dev]\n      tools: [query]\n"), ""},
		{"narrows", osv("      egress: [api.osv.dev, b.example]\n      secrets: [OSV_TOKEN]\n"), osv("      egress: [api.osv.dev]\n      tools: [query]\n"), ""},
		{"top-level narrows", "capabilities:\n  network:\n    egress: [api.example]\n", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(ArchiveWidenings(caps(t, tc.consented), caps(t, tc.archive)), "; ")
			if tc.want == "" {
				if got != "" {
					t.Fatalf("reported a widening for an archive within the contract: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("widenings %q do not say %q", got, tc.want)
			}
		})
	}
}

// TestShippedKnowledgeIsInTheSnapshot: the packs a release ships are part
// of the recorded contract, so a later release shipping one more re-asks
// and the record reads back what was consented (agentry-cli#320).
func TestShippedKnowledgeIsInTheSnapshot(t *testing.T) {
	before := Snapshot(manifest.Capabilities{})
	after := Snapshot(manifest.Capabilities{Knowledge: []string{"aws-docs"}})
	if !slices.Equal(after.Knowledge, []string{"aws-docs"}) {
		t.Fatalf("snapshot = %+v", after)
	}
	if d := DiffCapabilities(before, after); !slices.Contains(d.Widened, "ships knowledge: aws-docs") {
		t.Fatalf("a newly shipped pack did not widen: %+v", d)
	}
	if d := DiffCapabilities(after, after); !d.Empty() {
		t.Fatalf("the same packs changed the contract: %+v", d)
	}
	if got := Snapshot(after.capabilities()); !slices.Equal(got.Knowledge, after.Knowledge) {
		t.Fatalf("round trip = %+v", got)
	}
}

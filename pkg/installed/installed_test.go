package installed

// The capability diff. The whole value of the install manifest is what it
// makes the client *ask* about, so these tests are about which changes count
// as widening.

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
	"gopkg.in/yaml.v3"

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

// TestSnapshotAndDiffOfServerEnv: a declared server's plain env is part of
// the installed contract. A new or changed value is a change to the server
// and re-asks for consent; a dropped one narrows; the same env twice is no
// diff, and a record written before env existed compares as "no env" rather
// than as corrupt.
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
	if !strings.Contains(strings.Join(fresh.Widened, "|"), "adds MCP server: npm:github-mcp-server-js@0.1.7 (env GITHUB_PERMISSION=read-only, LOGS_DIR=/tmp/logs; secrets GITHUB_TOKEN; every tool it serves)") {
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
// rule needs: the diff is the core's manifest.DiffCapabilities, over
// manifest.Capabilities, so a stored snapshot is turned back into that type
// first. Snapshotting the result must give back the snapshot exactly — every
// field, a scoped npm server whose name carries its own "@", a PyPI server,
// and a server's env — or an installed record would read as a changed
// contract and re-ask.
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
		MCPServerCaps: map[string]ServerCapsSnapshot{
			"npm:@scope/server@1.2.3": {Egress: []string{"api.scope.example"}, Secrets: []string{"SCOPE_TOKEN"}, Tools: []string{"query"}},
			"pypi:mcp-thing@0.4.0":    {},
		},
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

// TestArchiveWideningsHoldTheArchiveToTheConsentedContract: what an archive's
// own agent.yaml declares beyond the resolve manifest the person consented to
// — the core rule's widenings, a same-pin server's own egress, secrets and
// tools among them, each reported once. Matching and narrowing report
// nothing.
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
		{"server secret", osv(""), osv("      secrets: [OSV_TOKEN]\n"), "gives MCP server npm:@acme/osv-mcp@1.2.3 the secret OSV_TOKEN"},
		{"server tools lifted", osv("      tools: [query]\n"), osv(""), "lifts the tool allowlist of MCP server npm:@acme/osv-mcp@1.2.3"},
		{"server tool added", osv("      tools: [query]\n"), osv("      tools: [query, delete]\n"), "adds tool delete to MCP server npm:@acme/osv-mcp@1.2.3"},
		{"matches", osv("      egress: [api.osv.dev]\n      tools: [query]\n"), osv("      egress: [api.osv.dev]\n      tools: [query]\n"), ""},
		{"narrows", osv("      egress: [api.osv.dev, b.example]\n      secrets: [OSV_TOKEN]\n"), osv("      egress: [api.osv.dev]\n      tools: [query]\n"), ""},
		{"top-level narrows", "capabilities:\n  network:\n    egress: [api.example]\n", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			widened := ArchiveWidenings(caps(t, tc.consented), caps(t, tc.archive))
			if len(widened) > 1 {
				t.Fatalf("one widening reported as %d lines: %q", len(widened), widened)
			}
			got := strings.Join(widened, "; ")
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
// and the record reads back what was consented.
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

// TestTargetHostsAreInTheSnapshot: the target hosts a release asks for are
// part of the recorded contract, so an update that adds one asks again, the
// record reads back what was consented, and a record from before they were
// recorded asks once more.
func TestTargetHostsAreInTheSnapshot(t *testing.T) {
	before := Snapshot(manifest.Capabilities{})
	after := Snapshot(manifest.Capabilities{TargetHosts: []string{"server"}})
	if !slices.Equal(after.TargetHosts, []string{"server"}) {
		t.Fatalf("snapshot = %+v", after)
	}
	d := DiffCapabilities(before, after)
	if !d.HasWidened() || !slices.ContainsFunc(d.Widened, func(w string) bool { return strings.Contains(w, "target host") && strings.Contains(w, "server") }) {
		t.Fatalf("a new target host did not widen: %+v", d)
	}
	if d := DiffCapabilities(after, after); !d.Empty() {
		t.Fatalf("the same target hosts changed the contract: %+v", d)
	}
	if d := DiffCapabilities(after, before); d.HasWidened() || len(d.Narrowed) == 0 {
		t.Fatalf("dropping a target host = %+v, want a narrowing", d)
	}
	if got := Snapshot(after.capabilities()); !slices.Equal(got.TargetHosts, after.TargetHosts) {
		t.Fatalf("round trip = %+v", got)
	}
	var st State
	if err := yaml.Unmarshal([]byte("records:\n  - registry: r\n    namespace: n\n    name: a\n    target: runner\n    version: 1.0.0\n    capabilities:\n      egressNone: true\n"), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Records) != 1 || !DiffCapabilities(st.Records[0].Capabilities, after).HasWidened() {
		t.Fatalf("a record without targetHosts does not ask again for one: %+v", st)
	}
}

// TestArchiveWideningsCatchAShippedPackTheResolveDidNotName: an archive
// whose own agent.yaml ships a pack the consented contract did not name is
// a widening the install refuses.
func TestArchiveWideningsCatchAShippedPackTheResolveDidNotName(t *testing.T) {
	consented := manifest.Capabilities{Knowledge: []string{"aws-docs"}}
	archive := manifest.Capabilities{Knowledge: []string{"aws-docs", "extra"}}
	if got := ArchiveWidenings(consented, archive); !slices.Contains(got, "ships knowledge: extra") {
		t.Fatalf("widenings = %v", got)
	}
	if got := ArchiveWidenings(archive, consented); len(got) != 0 {
		t.Fatalf("a narrower archive widened: %v", got)
	}
}

// Reconsent keeps what an install recorded: a run of the local install
// keeps its mark and digest, a run of another tree records nothing over
// it, and a registry record stays a registry record.
func TestReconsentKeepsALocalInstall(t *testing.T) {
	var st State
	k := Key{Registry: "runner", Namespace: "me", Name: "x", Target: "runner", Dir: "/h/agents/me/x"}
	st.Upsert(k, "0.1.0", CapSnapshot{}, true)
	st.MarkLocal(k, "sha256:aaa")
	if !st.Reconsent(k, "0.1.0", CapSnapshot{}, "sha256:aaa") {
		t.Fatal("a run of the local install recorded nothing")
	}
	if r := st.Find(k); !r.Local() || r.Digest != "sha256:aaa" || !r.Managed {
		t.Fatalf("after a run of the local install: %+v", r)
	}
	if st.Reconsent(k, "0.2.0", CapSnapshot{}, "sha256:bbb") {
		t.Fatal("a run of another tree overwrote the local install's record")
	}
	if r := st.Find(k); !r.Local() || r.Digest != "sha256:aaa" || r.Version != "0.1.0" {
		t.Fatalf("after a run of another tree: %+v", r)
	}
	st.Upsert(k, "1.0.0", CapSnapshot{}, true)
	if r := st.Find(k); r.Local() || r.Digest != "" {
		t.Fatalf("an install from a registry kept the local mark: %+v", r)
	}
	if !st.Reconsent(k, "1.0.0", CapSnapshot{}, "sha256:ccc") || st.Find(k).Local() {
		t.Fatal("a registry record became local")
	}
}

// A remote server in the consent record: the record keeps its address,
// auth, scopes, tools and bearer secret, so the same release asks nothing
// again, while a changed auth, a new tool or a lifted allowlist re-asks
// through the core's rule.
func TestSnapshotAndDiffOfRemoteServers(t *testing.T) {
	parse := func(servers string) manifest.Capabilities {
		t.Helper()
		m, err := manifest.Parse([]byte(`schemaVersion: "2"
name: snap
namespace: demo
modes: [autonomous]
version: 1.0.0
summary: "remote snapshot fixture"
capabilities:
  secrets: [GITHUB_TOKEN]
  mcpServers:
` + servers + `
publish:
  include: [agent.yaml]
`))
		if err != nil {
			t.Fatal(err)
		}
		return m.Capabilities
	}
	base := parse(`    - url: https://api.githubcopilot.com/mcp/readonly
      auth: secret
      secrets: [GITHUB_TOKEN]
      tools: [get_me]
    - url: https://gmailmcp.googleapis.com/mcp/v1
      auth: oauth
      scopes: [https://www.googleapis.com/auth/gmail.readonly]
    - pypi: workspace-mcp
      version: 2.0.1
      oauth:
        issuer: https://accounts.google.com
        env: {GOOGLE_TOKEN: "{access_token}"}`)
	snap := Snapshot(base)
	if r := snap.MCPRemote["url:https://api.githubcopilot.com/mcp/readonly"]; r.Auth != "secret" || !slices.Equal(r.Secrets, []string{"GITHUB_TOKEN"}) || !slices.Equal(r.Tools, []string{"get_me"}) {
		t.Fatalf("the record misses the remote server: %+v", snap.MCPRemote)
	}
	if d := DiffCapabilities(snap, Snapshot(base)); !d.Empty() {
		t.Fatalf("the same release read as a changed contract: %+v", d)
	}
	if !reflect.DeepEqual(Snapshot(snap.capabilities()), snap) {
		t.Fatalf("capabilities() does not invert Snapshot for remote servers:\n%+v\n%+v", Snapshot(snap.capabilities()), snap)
	}
	for _, c := range []struct{ servers, want string }{
		{`    - url: https://api.githubcopilot.com/mcp/readonly
      auth: none
      tools: [get_me]`, "changes auth of MCP server url:https://api.githubcopilot.com/mcp/readonly"},
		{`    - url: https://api.githubcopilot.com/mcp/readonly
      auth: secret
      secrets: [GITHUB_TOKEN]`, "lifts the tool allowlist"},
		{`    - url: https://api.githubcopilot.com/mcp/readonly
      auth: secret
      secrets: [GITHUB_TOKEN]
      tools: [get_me, create_issue]`, "adds tool create_issue"},
	} {
		first := Snapshot(parse(`    - url: https://api.githubcopilot.com/mcp/readonly
      auth: secret
      secrets: [GITHUB_TOKEN]
      tools: [get_me]`))
		d := DiffCapabilities(first, Snapshot(parse(c.servers)))
		if !strings.Contains(strings.Join(d.Widened, "|"), c.want) {
			t.Errorf("want a widening %q, got %+v", c.want, d)
		}
	}
}

// TestTheBrowserIsInTheSnapshot: the browser line is part of the recorded
// contract, so an update that adds one, a host, a secret it may type or site
// to public asks again, the record reads back what was consented and survives
// YAML.
func TestTheBrowserIsInTheSnapshot(t *testing.T) {
	site := manifest.Capabilities{Browser: &manifest.Browser{Sites: []string{"site"}, Hosts: []string{"cdn.example.com"}}}
	before, after := Snapshot(manifest.Capabilities{}), Snapshot(site)
	if after.Browser == nil || !slices.Equal(after.Browser.Hosts, []string{"cdn.example.com"}) {
		t.Fatalf("snapshot = %+v", after)
	}
	if d := DiffCapabilities(before, after); !d.HasWidened() {
		t.Fatalf("a new browser did not widen: %+v", d)
	}
	if d := DiffCapabilities(after, after); !d.Empty() {
		t.Fatalf("the same browser changed the contract: %+v", d)
	}
	for name, next := range map[string]*manifest.Browser{
		"a host":   {Sites: []string{"site"}, Hosts: []string{"cdn.example.com", "*.cdnhost.net"}},
		"a secret": {Sites: []string{"site"}, Hosts: []string{"cdn.example.com"}, Secrets: []string{"SITE_PASSWORD"}},
		"public":   {Sites: []string{"site"}, Hosts: []string{"cdn.example.com"}, Scope: manifest.BrowserScopePublic},
		"a site":   {Sites: []string{"site", "staging"}, Hosts: []string{"cdn.example.com"}},
	} {
		if d := DiffCapabilities(after, Snapshot(manifest.Capabilities{Browser: next})); !d.HasWidened() {
			t.Errorf("%s did not widen: %+v", name, d)
		}
	}
	b, err := yaml.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	var back CapSnapshot
	if err := yaml.Unmarshal(b, &back); err != nil || !DiffCapabilities(after, back).Empty() {
		t.Fatalf("YAML round trip: %s %v %+v", b, err, back)
	}
	if got := Snapshot(after.capabilities()); !DiffCapabilities(after, got).Empty() {
		t.Fatalf("round trip = %+v", got)
	}
}

// osvServerCaps parses a contract declaring one package server, with the
// given extra lines of that server's declaration.
func osvServerCaps(t *testing.T, extra string) manifest.Capabilities {
	t.Helper()
	m, err := manifest.Parse([]byte("schemaVersion: \"2\"\nname: w\nnamespace: demo\nmodes: [autonomous]\nversion: 1.0.0\nsummary: s\n" +
		"capabilities:\n  mcpServers:\n    - npm: \"@acme/osv-mcp\"\n      version: 1.2.3\n" + extra))
	if err != nil {
		t.Fatal(err)
	}
	return m.Capabilities
}

const osvID = "npm:@acme/osv-mcp@1.2.3"

// TestAServersOwnGrantsAreInTheSnapshot: a package server's own egress,
// secrets and tools are recorded and read back through installed.yaml, so
// an update that keeps the server's pin and widens them asks again.
func TestAServersOwnGrantsAreInTheSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv(configdir.EnvVar, filepath.Join(home, "config"))

	consented := Snapshot(osvServerCaps(t, "      egress: [a.example]\n      secrets: [OSV_TOKEN]\n      tools: [query]\n"))
	want := ServerCapsSnapshot{Egress: []string{"a.example"}, Secrets: []string{"OSV_TOKEN"}, Tools: []string{"query"}}
	if got := consented.MCPServerCaps[osvID]; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot of the server's grants = %+v, want %+v", got, want)
	}
	bare := Snapshot(osvServerCaps(t, ""))
	if got, ok := bare.MCPServerCaps[osvID]; !ok || !reflect.DeepEqual(got, ServerCapsSnapshot{}) {
		t.Fatalf("a server declaring no grants has no recorded entry: %+v", bare.MCPServerCaps)
	}

	key := Key{Registry: "https://reg.example", Namespace: "demo", Name: "w", Target: "runner", Dir: "/x"}
	var st State
	st.Upsert(key, "1.0.0", consented, true)
	st.Upsert(Key{Registry: key.Registry, Namespace: "demo", Name: "bare", Target: "runner", Dir: "/y"}, "1.0.0", bare, true)
	if err := Save(st); err != nil {
		t.Fatal(err)
	}
	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := back.Find(key)
	if rec == nil || !reflect.DeepEqual(rec.Capabilities.MCPServerCaps, consented.MCPServerCaps) {
		t.Fatalf("the server's grants did not survive installed.yaml: %+v", rec)
	}
	if b := back.Find(Key{Registry: key.Registry, Namespace: "demo", Name: "bare", Target: "runner", Dir: "/y"}); b == nil || !DiffCapabilities(b.Capabilities, bare).Empty() {
		t.Fatalf("a server with no grants read back as a different contract: %+v", b)
	}
	if d := DiffCapabilities(rec.Capabilities, consented); !d.Empty() {
		t.Fatalf("the same contract after a reload is a diff: %+v", d)
	}

	for _, tc := range []struct{ name, next, want string }{
		{"egress", "      egress: [a.example, b.example]\n      secrets: [OSV_TOKEN]\n      tools: [query]\n", "adds network egress to b.example for MCP server " + osvID},
		{"secret", "      egress: [a.example]\n      secrets: [OSV_TOKEN, DB_URL]\n      tools: [query]\n", "gives MCP server " + osvID + " the secret DB_URL"},
		{"tool", "      egress: [a.example]\n      secrets: [OSV_TOKEN]\n      tools: [query, delete]\n", "adds tool delete to MCP server " + osvID},
		{"allowlist lifted", "      egress: [a.example]\n      secrets: [OSV_TOKEN]\n", "lifts the tool allowlist of MCP server " + osvID + " (every tool it serves)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DiffCapabilities(rec.Capabilities, Snapshot(osvServerCaps(t, tc.next)))
			if !d.HasWidened() || !slices.Contains(d.Widened, tc.want) {
				t.Fatalf("a same-pin widening was not asked about as %q: %+v", tc.want, d)
			}
		})
	}
	if d := DiffCapabilities(rec.Capabilities, Snapshot(osvServerCaps(t, "      tools: [query]\n"))); d.HasWidened() {
		t.Fatalf("a narrower server widened: %+v", d)
	}
}

// TestARecordWithoutAServersGrantsAsksOnce: a record that names a server
// without its own grants does not know them. It reads as neither
// narrower nor equal: the update asks once, naming what the server is
// given, and the record the consent writes then compares as equal.
func TestARecordWithoutAServersGrantsAsksOnce(t *testing.T) {
	var st State
	if err := yaml.Unmarshal([]byte("records:\n  - registry: r\n    namespace: demo\n    name: w\n    target: runner\n    version: 1.0.0\n    capabilities:\n      mcpServers: [\""+osvID+"\"]\n"), &st); err != nil {
		t.Fatal(err)
	}
	old := st.Records[0].Capabilities
	for _, tc := range []struct{ name, next, want string }{
		{"declares grants", "      egress: [a.example]\n      secrets: [OSV_TOKEN]\n      tools: [query]\n",
			"asks again for MCP server " + osvID + ", whose own grants your last consent did not record: network egress to a.example; secrets OSV_TOKEN; tools query"},
		{"declares none", "",
			"asks again for MCP server " + osvID + ", whose own grants your last consent did not record: network egress to no host; secrets none; tools every tool it serves"},
		{"any host", "      egress: [\"*\"]\n", "network egress to any host (UNRESTRICTED)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := Snapshot(osvServerCaps(t, tc.next))
			d := DiffCapabilities(old, next)
			if !d.HasWidened() || !slices.ContainsFunc(d.Widened, func(w string) bool { return strings.Contains(w, tc.want) }) {
				t.Fatalf("an unrecorded server's grants did not ask again as %q: %+v", tc.want, d)
			}
			if len(d.Widened) != 1 || len(d.Narrowed) != 0 {
				t.Fatalf("an unrecorded server reported more than its one line: %+v", d)
			}
			if d := DiffCapabilities(next, next); !d.Empty() {
				t.Fatalf("the record written by the consent still differs: %+v", d)
			}
		})
	}
	// A remote server's grants are recorded beside its address, so its
	// record is never unknown.
	remote := CapSnapshot{MCPServers: []string{"url:https://mcp.example/sse"}, MCPRemote: map[string]RemoteSnapshot{"url:https://mcp.example/sse": {Auth: "none"}}}
	if d := DiffCapabilities(remote, remote); !d.Empty() {
		t.Fatalf("a remote server read as unrecorded: %+v", d)
	}
}

// TestAServersArgsAreInTheSnapshot: the args a consent agreed to are
// recorded with the server's own grants and read back unchanged, and an
// update at the same pin that changes them, drops one or adds one asks
// again. A record that has none of the server's grants names its args in
// the one line that asks again.
func TestAServersArgsAreInTheSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv(configdir.EnvVar, filepath.Join(home, "config"))
	consented := Snapshot(osvServerCaps(t, "      egress: [a.example]\n      args: [mcp, --read-only]\n"))
	if got := consented.MCPServerCaps[osvID].Args; !slices.Equal(got, []string{"mcp", "--read-only"}) {
		t.Fatalf("snapshot args = %q", got)
	}
	key := Key{Registry: "https://reg.example", Namespace: "demo", Name: "w", Target: "runner", Dir: "/x"}
	var st State
	st.Upsert(key, "1.0.0", consented, true)
	if err := Save(st); err != nil {
		t.Fatal(err)
	}
	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := back.Find(key)
	if rec == nil || !DiffCapabilities(rec.Capabilities, consented).Empty() {
		t.Fatalf("the args did not survive installed.yaml: %+v", rec)
	}
	for _, next := range []string{
		"      egress: [a.example]\n      args: [mcp]\n",
		"      egress: [a.example]\n      args: [mcp, --read-only, --allow-write]\n",
		"      egress: [a.example]\n",
	} {
		d := DiffCapabilities(rec.Capabilities, Snapshot(osvServerCaps(t, next)))
		if !d.HasWidened() || !slices.ContainsFunc(d.Widened, func(w string) bool { return strings.Contains(w, "changes args of MCP server "+osvID) }) {
			t.Fatalf("a change of args to %q was not asked about: %+v", next, d)
		}
	}
	old := CapSnapshot{MCPServers: []string{osvID}}
	d := DiffCapabilities(old, consented)
	if len(d.Widened) != 1 || !strings.HasSuffix(d.Widened[0], "; args mcp --read-only") {
		t.Fatalf("an unrecorded server's line does not name its args: %+v", d)
	}
}

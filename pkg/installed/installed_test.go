package installed

// The capability diff (§11 install row). The whole value of the install
// manifest is what it makes the client *ask* about, so these tests are about
// which changes count as widening.

import (
	"path/filepath"
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

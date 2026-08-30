package main

// The capability diff (§11 install row). The whole value of the install
// manifest is what it makes the CLI *ask* about, so these tests are about
// which changes count as widening.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffCapabilitiesWidening(t *testing.T) {
	tests := []struct {
		name     string
		old, new capSnapshot
		widened  bool
		contains string
	}{
		{
			name:     "new egress host",
			old:      capSnapshot{EgressNone: true},
			new:      capSnapshot{EgressHosts: []string{"evil.example"}},
			widened:  true,
			contains: "adds network egress to evil.example",
		},
		{
			name:     "wildcard egress is called out as unrestricted",
			old:      capSnapshot{EgressHosts: []string{"api.example"}},
			new:      capSnapshot{EgressHosts: []string{"api.example", "*"}},
			widened:  true,
			contains: "UNRESTRICTED",
		},
		{
			name:     "exec none to commands",
			old:      capSnapshot{},
			new:      capSnapshot{ExecCommands: []string{"git"}},
			widened:  true,
			contains: "adds subprocess execution: git (previously none)",
		},
		{
			name:     "filesystem write escalation",
			old:      capSnapshot{FSWrite: "none"},
			new:      capSnapshot{FSWrite: "workspace"},
			widened:  true,
			contains: "widens filesystem write from none to workspace",
		},
		{
			name:     "a new explicit write path",
			old:      capSnapshot{FSWrite: "paths", FSWritePaths: []string{"out"}},
			new:      capSnapshot{FSWrite: "paths", FSWritePaths: []string{"out", "/etc"}},
			widened:  true,
			contains: "adds filesystem write path: /etc",
		},
		{
			name:     "a new secret",
			old:      capSnapshot{Secrets: []string{"API_KEY"}},
			new:      capSnapshot{Secrets: []string{"API_KEY", "AWS_SECRET_ACCESS_KEY"}},
			widened:  true,
			contains: "asks for a new secret: AWS_SECRET_ACCESS_KEY",
		},
		{
			name:     "new data access",
			old:      capSnapshot{},
			new:      capSnapshot{DataAccess: []string{"source-code"}},
			widened:  true,
			contains: "adds data access: source-code",
		},
		{
			name:     "dropping an egress host is a narrowing",
			old:      capSnapshot{EgressHosts: []string{"a.example", "b.example"}},
			new:      capSnapshot{EgressHosts: []string{"a.example"}},
			widened:  false,
			contains: "drops network egress to b.example",
		},
		{
			name:     "narrowing write access",
			old:      capSnapshot{FSWrite: "workspace"},
			new:      capSnapshot{FSWrite: "none"},
			widened:  false,
			contains: "narrows filesystem write from workspace to none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := diffCapabilities(tt.old, tt.new)
			if d.widened() != tt.widened {
				t.Fatalf("widened = %v, want %v (widened=%v narrowed=%v)",
					d.widened(), tt.widened, d.Widened, d.Narrowed)
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
	c := capSnapshot{
		EgressHosts: []string{"api.example"}, ExecCommands: []string{"git"},
		FSRead: "workspace", FSWrite: "paths", FSWritePaths: []string{"out"},
		Secrets: []string{"API_KEY"}, DataAccess: []string{"source-code"},
	}
	if d := diffCapabilities(c, c); !d.empty() {
		t.Fatalf("identical contracts diffed: %+v", d)
	}
}

// TestUndeclaredIsNotAWideningOverExplicitNone guards the ranking: a manifest
// that stops declaring anything is a step DOWN from explicit "none", but it
// is not a step up into access.
func TestUndeclaredVsExplicitNone(t *testing.T) {
	toUndeclared := diffCapabilities(capSnapshot{FSRead: "none"}, capSnapshot{FSRead: ""})
	if !toUndeclared.widened() {
		t.Error("none -> undeclared should be reported as a widening (we know less than before)")
	}
	toWorkspace := diffCapabilities(capSnapshot{FSRead: ""}, capSnapshot{FSRead: "workspace"})
	if !toWorkspace.widened() {
		t.Error("undeclared -> workspace must be a widening")
	}
	toNone := diffCapabilities(capSnapshot{FSRead: ""}, capSnapshot{FSRead: "none"})
	if toNone.widened() {
		t.Error("undeclared -> none is a narrowing, not a widening")
	}
}

// TestInstallStateRoundTrip: the record must survive YAML, including the
// union-shaped capability fields that a naive marshal of manifest.Capabilities
// would silently drop.
func TestInstallStateRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv(envConfigDir, filepath.Join(home, "config"))

	key := installKey{
		registry: "https://reg.example/", namespace: "acme", name: "helper",
		target: "claude-skill", dir: "/tmp/skills/helper",
	}
	caps := capSnapshot{
		EgressHosts: []string{"api.example"}, FSWrite: "paths",
		FSWritePaths: []string{"out"}, Secrets: []string{"API_KEY"},
	}
	var st installState
	st.record(key, "1.0.0", caps)
	if err := saveInstallState(st); err != nil {
		t.Fatal(err)
	}

	back, err := loadInstallState()
	if err != nil {
		t.Fatal(err)
	}
	rec := back.findInstall(key)
	if rec == nil {
		t.Fatalf("record not found after reload: %+v", back.Records)
	}
	if rec.Version != "1.0.0" {
		t.Errorf("version = %q", rec.Version)
	}
	if d := diffCapabilities(rec.Capabilities, caps); !d.empty() {
		t.Errorf("capabilities did not survive the round trip: %+v", d)
	}

	// A second record for the SAME agent at a different directory is separate:
	// two installations have independent capability histories.
	other := key
	other.dir = "/other/place"
	back.record(other, "2.0.0", capSnapshot{})
	if len(back.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(back.Records))
	}
	if back.findInstall(key).Version != "1.0.0" {
		t.Error("recording a second directory overwrote the first")
	}
}

// TestResolveInstallPathsManagedFlag: only a directory the CLI chose is
// cleared on reinstall.
func TestResolveInstallPathsManagedFlag(t *testing.T) {
	for _, target := range validTargets {
		withDir, err := resolveInstallPaths(target, "/user/chosen", "helper")
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if withDir.managed {
			t.Errorf("%s: a --dir destination is marked managed and would be cleared", target)
		}
		def, err := resolveInstallPaths(target, "", "helper")
		if err != nil {
			t.Fatalf("%s default: %v", target, err)
		}
		if !def.managed {
			t.Errorf("%s: the default destination is not managed and stale files would linger", target)
		}
	}
	if _, err := resolveInstallPaths("nope", "", "helper"); err == nil {
		t.Error("an unknown target resolved to a path")
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
)

// list --check asks for the advisories on each registry release held here,
// presenting its digest, and reads them even when the QuickAgent no longer
// resolves (gone private, retired, deleted). A QuickAgent the registry does
// not know is said as such, and a lookup that fails says it could not be
// checked, never that there are none.
func TestListCheckReportsTheHeldReleasesAdvisories(t *testing.T) {
	isolateEnv(t)
	home, _ := isolateRunnerHome(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	if _, stderr, err := runCLI(t, nil, "--registry", fr.url(), "install", "acme/demo@1.0.0", "--allow-unsigned"); err != nil {
		t.Fatalf("install: %v (stderr %q)", err, stderr)
	}
	digest := heldDigest(home, "acme", "demo", "1.0.0")
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("the held release's digest = %q", digest)
	}
	// What proves the holding is the digest the release's own folder is named
	// for: a record naming another digest, or one that is no sha256, holds
	// nothing.
	agentDir := installdir.AgentDir(home, "acme", "demo")
	record := func(dir, version, digest string) {
		t.Helper()
		raw, err := json.Marshal(installdir.Meta{Version: version, Digest: digest})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(agentDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, dir, "install.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record("2.0.0-"+strings.Repeat("c", 12), "2.0.0", "sha256:"+strings.Repeat("d", 64))
	record("3.0.0-abc123def456", "3.0.0", "abc123def456abc")
	record("4.0.0-"+strings.Repeat("e", 12), "4.0.0", "sha256:"+strings.Repeat("e", 64))
	for version, want := range map[string]string{"1.0.0": digest, "2.0.0": "", "3.0.0": "", "4.0.0": "sha256:" + strings.Repeat("e", 64), "5.0.0": ""} {
		if got := heldDigest(home, "acme", "demo", version); got != want {
			t.Errorf("heldDigest(%s) = %q, want %q", version, got, want)
		}
	}
	// The QuickAgent goes private: it no longer resolves, but a holder of
	// the release still reads its advisory.
	fr.mu.Lock()
	delete(fr.agents, "acme/demo")
	fr.advisories = map[string][]map[string]any{"acme/demo": {{
		"id": "01a00000-0000-7000-8000-000000000001", "severity": "high", "summary": "exfiltrates the workspace",
		"affectedVersions": "<1.1.0", "publishedAt": "2026-10-16T00:00:00Z",
	}, {
		"id": "01a00000-0000-7000-8000-000000000003", "severity": "low", "summary": "leaks a path",
		"affectedVersions": "<1.0.1", "publishedAt": "2026-10-17T00:00:00Z",
	}}}
	fr.mu.Unlock()

	listed := func() listedRecord {
		t.Helper()
		stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "--json", "list", "--check")
		if err != nil {
			t.Fatal(err)
		}
		var out listOutput
		if err := json.Unmarshal([]byte(stdout), &out); err != nil || len(out.Records) != 1 {
			t.Fatalf("listing: %v\n%s", err, stdout)
		}
		return out.Records[0]
	}
	r := listed()
	if len(r.Advisories) != 2 || r.Advisories[0].Severity != "high" || r.AdvisoriesError != "" || r.CheckError == "" {
		t.Fatalf("record: %+v", r)
	}
	fr.mu.Lock()
	q := fr.advisoryQueries[len(fr.advisoryQueries)-1]
	fr.mu.Unlock()
	if q.Get("affecting") != "acme/demo" || q.Get("digest") != digest {
		t.Fatalf("advisories asked with %v, want affecting acme/demo and digest %s", q, digest)
	}
	stdout, _, _ := runCLI(t, nil, "--registry", fr.url(), "list", "--check")
	if !strings.Contains(stdout, "advisories: high: exfiltrates the workspace (affects <1.1.0); low: leaks a path (affects <1.0.1)") {
		t.Fatalf("text listing:\n%s", stdout)
	}

	// None on it.
	fr.mu.Lock()
	fr.advisories["acme/demo"] = []map[string]any{}
	fr.mu.Unlock()
	if r := listed(); len(r.Advisories) != 0 || r.AdvisoriesUnknown || r.AdvisoriesError != "" {
		t.Fatalf("no advisories: %+v", r)
	}
	if stdout, _, _ := runCLI(t, nil, "--registry", fr.url(), "list", "--check"); !strings.Contains(stdout, "no advisories") {
		t.Fatalf("text listing:\n%s", stdout)
	}

	// Not known to the registry: said so, not an error that stops the list.
	fr.mu.Lock()
	fr.advisories = map[string][]map[string]any{}
	fr.mu.Unlock()
	if r := listed(); !r.AdvisoriesUnknown || r.AdvisoriesError != "" {
		t.Fatalf("an unknown QuickAgent: %+v", r)
	}
	if stdout, _, _ := runCLI(t, nil, "--registry", fr.url(), "list", "--check"); !strings.Contains(stdout, "not known to the registry") {
		t.Fatalf("text listing:\n%s", stdout)
	}

	// A lookup that fails could not be checked.
	fr.mu.Lock()
	fr.advisoryStatus = 503
	fr.mu.Unlock()
	if r := listed(); r.AdvisoriesError != "503 Service Unavailable: unavailable" || len(r.Advisories) != 0 {
		t.Fatalf("a failed lookup: %+v", r)
	}
	if stdout, _, _ := runCLI(t, nil, "--registry", fr.url(), "list", "--check"); !strings.Contains(stdout, "advisories could not be checked") || strings.Contains(stdout, "no advisories") {
		t.Fatalf("text listing:\n%s", stdout)
	}
}

// doctor reports the advisories on the release it checks, with its digest,
// and says a failed lookup as could not be checked.
func TestDoctorReportsTheHeldReleasesAdvisories(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	old := flags.registry
	t.Cleanup(func() { flags.registry = old })
	flags.registry = fr.url()
	meta := &installdir.Meta{Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: "sha256:" + strings.Repeat("ab", 32)}
	fr.advisories = map[string][]map[string]any{"acme/demo": {{
		"id": "01a00000-0000-7000-8000-000000000002", "severity": "critical", "summary": "runs a remote script", "affectedVersions": "*",
	}}}
	rep := &doctorReport{}
	doctorAdvisories(rep, meta)
	if !doctorHas(rep, "warn", "acme/demo@1.0.0: critical advisory: runs a remote script (affects *)") {
		t.Fatalf("doctor: %+v", rep.Checks)
	}
	if q := fr.advisoryQueries[len(fr.advisoryQueries)-1]; q.Get("digest") != meta.Digest {
		t.Fatalf("doctor asked with %v", q)
	}
	fr.advisoryStatus = 500
	rep = &doctorReport{}
	doctorAdvisories(rep, meta)
	if !doctorHas(rep, "warn", "advisories could not be checked") || doctorHas(rep, "ok", "no advisories") {
		t.Fatalf("doctor on a failed lookup: %+v", rep.Checks)
	}
	// A QuickAgent the registry does not know is said so, and one with no
	// advisory has none.
	fr.advisoryStatus = 0
	fr.advisories = map[string][]map[string]any{}
	rep = &doctorReport{}
	doctorAdvisories(rep, meta)
	if !doctorHas(rep, "warn", "acme/demo@1.0.0: not known to the registry") {
		t.Fatalf("doctor on an unknown QuickAgent: %+v", rep.Checks)
	}
	fr.advisories = map[string][]map[string]any{"acme/demo": {}}
	rep = &doctorReport{}
	doctorAdvisories(rep, meta)
	if !doctorHas(rep, "ok", "acme/demo@1.0.0: no advisories") {
		t.Fatalf("doctor on a QuickAgent with none: %+v", rep.Checks)
	}
	// A QuickAgent installed from a folder has no registry to ask.
	before := len(fr.advisoryQueries)
	doctorAdvisories(&doctorReport{}, &installdir.Meta{Namespace: "acme", Name: "demo", Source: "local"})
	if len(fr.advisoryQueries) != before {
		t.Fatal("a local install's advisories were asked of the registry")
	}
}

// `agentry yank`: exact pins only, credential required before the request,
// the recorded reason travels on the wire, and problems render as errors.
package main

import (
	"strings"
	"testing"
)

func TestYankHappyPathForwardsReason(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envToken, "agy_testtoken")
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	stdout, _, err := runCLI(t, nil, "--registry", fr.url(), "yank", "acme/demo@1.0.0", "--reason", "CVE-2026-0001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "yanked acme/demo@1.0.0 (reason: CVE-2026-0001)") {
		t.Errorf("yank output = %q", stdout)
	}
	// The reason is recorded and shown to installers — it must actually be
	// in the request body, not only in local output.
	if len(fr.yankBodies) != 1 || !strings.Contains(fr.yankBodies[0], `"CVE-2026-0001"`) {
		t.Errorf("yank bodies = %v", fr.yankBodies)
	}
}

func TestYankGuards(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	// --reason is required by the flag layer itself.
	_, _, err := runCLI(t, nil, "--registry", fr.url(), "yank", "acme/demo@1.0.0")
	if err == nil || !strings.Contains(err.Error(), "reason") {
		t.Errorf("missing --reason: %v", err)
	}

	// Exact pins only: yanking "whatever currently resolves" is how the
	// wrong version gets burned.
	t.Setenv(envToken, "agy_testtoken")
	_, _, err = runCLI(t, nil, "--registry", fr.url(), "yank", "acme/demo", "--reason", "r")
	if err == nil || !strings.Contains(err.Error(), "exact version") {
		t.Errorf("range yank: %v", err)
	}

	// No credential: refused before any HTTP, naming both fixes.
	t.Setenv(envToken, "")
	_, _, err = runCLI(t, nil, "--registry", fr.url(), "yank", "acme/demo@1.0.0", "--reason", "r")
	if err == nil || !strings.Contains(err.Error(), "agentry login") || !strings.Contains(err.Error(), envToken) {
		t.Errorf("anonymous yank: %v", err)
	}

	// A server-side refusal renders the problem document.
	t.Setenv(envToken, "agy_wrongtoken")
	_, _, err = runCLI(t, nil, "--registry", fr.url(), "yank", "acme/demo@1.0.0", "--reason", "r")
	if err == nil || !strings.Contains(err.Error(), "forbidden") || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("forbidden yank: %v", err)
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// The CLI's half of TestOnlyTheEmbeddedPackagesRunAsBuiltin (plan
// knowledge.md §3; agentry-cli#318): no record on disk can make a release
// or a directory run as built in.
func TestNoCachedRecordRunsAsBuiltin(t *testing.T) {
	t.Run("a cached release claiming builtin is refused", func(t *testing.T) {
		meta := &installdir.Meta{Ref: "acme/x@1.0.0", Namespace: "acme", Name: "x", Attestation: receipt.Attestation{State: trust.StateBuiltin}}
		err := cachedAttestationGate(meta, true, true)
		if err == nil || !strings.Contains(err.Error(), "built in") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a forged record under a --local package does not change its state", func(t *testing.T) {
		isolateEnv(t)
		home, _ := isolateRunnerHome(t)
		pkg := repoPath(t, "testdata/runner/pure-text")
		meta, installDir, err := materializeLocal(stubCmd(t), pkg, home, true)
		if err != nil {
			t.Fatal(err)
		}
		forged := *meta
		forged.Attestation = receipt.Attestation{State: trust.StateBuiltin, Verified: true}
		b, _ := json.Marshal(forged)
		if err := os.WriteFile(filepath.Join(installDir, "install.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		again, _, err := materializeLocal(stubCmd(t), pkg, home, true)
		if err != nil {
			t.Fatal(err)
		}
		if again.Attestation.State != "local" || again.Attestation.Verified {
			t.Fatalf("a forged record gave the local package %+v", again.Attestation)
		}
	})
}

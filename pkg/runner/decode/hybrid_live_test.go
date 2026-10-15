package decode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/hybrid"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// eicarTest is the EICAR anti-malware test file: harmless, and known to
// every engine (SHA-256 275a021b…).
const eicarTest = `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`

// TestHybridAnalysisForReal decodes with the real decoders and looks the risky
// files up on Hybrid Analysis with a real key: EICAR's hash refuses, a clean
// PDF passes, a .txt is never asked about. It decodes with no ClamAV
// signatures, so EICAR reaches the lookup instead of being found first. It runs
// only with AGENTRY_HYBRID_E2E=1 and a key in $HYBRID_API_KEY.
func TestHybridAnalysisForReal(t *testing.T) {
	if os.Getenv("AGENTRY_HYBRID_E2E") != "1" || os.Getenv("HYBRID_API_KEY") == "" {
		t.Skip("set AGENTRY_HYBRID_E2E=1 and HYBRID_API_KEY to look up on Hybrid Analysis for real")
	}
	ctx := context.Background()
	d := dockerx.New()
	pol := policy.Default()
	home := t.TempDir()
	tag := pol.RunnerImageTag(runnerimage.ContentVersion())
	if err := gateway.EnsureInstalled(ctx, d, pol, tag, filepath.Join(home, "image"), os.Stderr, false); err != nil {
		t.Fatal(err)
	}
	img, err := Ensure(ctx, d, tag, pol, filepath.Join(home, "image"), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	in, out := t.TempDir(), t.TempDir()
	pdf, err := os.ReadFile(filepath.Join("..", "..", "knowledge", "creator", "testdata", "corpus", "incident.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"eicar.com": []byte(eicarTest), "incident.pdf": pdf, "notes.txt": []byte("plain notes\n")} {
		if err := os.WriteFile(filepath.Join(in, name), body, 0o644); err != nil { //nolint:gosec // read by the decode container
			t.Fatal(err)
		}
	}
	res, err := Run(ctx, d, img, in, out, pol.Docker.Limits.WithDefaults(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := hybrid.FromPolicy(pol)
	if err != nil {
		t.Fatal(err)
	}
	res.LookUp(ctx, c)
	t.Logf("checks: %s", res.Checks())
	t.Logf("refusal: %s", res.Refusal())
	if got := res.Lookups["eicar.com"].Outcome; got != hybrid.Refused || !res.Refused() {
		t.Errorf("EICAR: %s", res.Lookups["eicar.com"])
	}
	if got := res.Lookups["incident.pdf"].Outcome; got == hybrid.Refused || got == hybrid.NotChecked {
		t.Errorf("a clean PDF: %s", res.Lookups["incident.pdf"])
	}
	if _, asked := res.Lookups["notes.txt"]; asked {
		t.Error("a .txt was looked up")
	}
}

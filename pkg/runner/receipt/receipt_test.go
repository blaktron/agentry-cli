// Envelope tests: the §R8 result envelope is the runner's public output
// contract (pipelines parse it), so its field names, the TextResult
// quoting, and the write-to-out-dir behavior are pinned here.
package receipt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTextResult(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // raw JSON bytes
	}{
		{"plain", "final answer", `"final answer"`},
		{"empty", "", `""`},
		{"needs quoting", "say \"hi\"\nline2\ttabbed", `"say \"hi\"\nline2\ttabbed"`},
		{"non-ascii survives", "héllo", `"héllo"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TextResult(tc.in)
			if string(got) != tc.want {
				t.Errorf("TextResult(%q) = %s, want %s", tc.in, got, tc.want)
			}
			// Whatever the bytes, they must round-trip to the input string.
			var back string
			if err := json.Unmarshal(got, &back); err != nil || back != tc.in {
				t.Errorf("round-trip = (%q, %v)", back, err)
			}
		})
	}
}

// fullEnvelope is a populated envelope for shape tests.
func fullEnvelope() *Envelope {
	return &Envelope{
		Agentry:  EnvelopeVersion,
		Status:   "ok",
		ExitCode: 0,
		RunID:    "cafe0000",
		Ref:      "demo/agent@1.2.3",
		Digest:   "sha256:abc",
		Attestation: Attestation{
			State: "verified", KeyID: "key-1", Verified: true,
		},
		CapabilitiesEnforced: map[string]string{"network": "no egress"},
		Harness:              Harness{Adapter: "docker-agent", Image: "docker/docker-agent:v1", Model: "anthropic/claude-sonnet-4-5"},
		Usage:                Usage{InputTokens: 100, OutputTokens: 40, CostUSD: 0.05, WallMS: 1234, Turns: 2},
		Result:               TextResult("done"),
		OutDir:               "/tmp/out",
		Receipts:             Receipts{Dir: "/tmp/out/receipts", RunPlanSHA256: "deadbeef"},
	}
}

func TestEnvelopeMarshalShape(t *testing.T) {
	b, err := fullEnvelope().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Error("marshaled envelope must end with a newline (it IS stdout in json mode)")
	}
	// The wire field names are the contract; pin the load-bearing ones.
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"agentry", "status", "exitCode", "runId", "ref", "digest",
		"attestation", "capabilities_enforced", "harness", "usage", "result", "outDir", "receipts"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("envelope JSON lacks %q", key)
		}
	}
	if doc["agentry"] != "run/v1" {
		t.Errorf("agentry = %v", doc["agentry"])
	}
	rec := doc["receipts"].(map[string]any)
	if rec["runPlanSha256"] != "deadbeef" {
		t.Errorf("runPlanSha256 = %v", rec["runPlanSha256"])
	}
	// error is omitempty: an ok envelope must not carry it.
	if _, present := doc["error"]; present {
		t.Error("ok envelope carries an error field")
	}
}

func TestEnvelopeWrite(t *testing.T) {
	dir := t.TempDir()
	env := fullEnvelope()
	if err := env.Write(dir); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "result.json")) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.RunID != env.RunID || back.Ref != env.Ref || string(back.Result) != string(env.Result) {
		t.Errorf("round-trip mismatch: %+v", back)
	}

	err = env.Write(filepath.Join(dir, "no", "such", "dir"))
	if err == nil || !strings.Contains(err.Error(), "write result.json") {
		t.Fatalf("unwritable out dir: err = %v", err)
	}
}

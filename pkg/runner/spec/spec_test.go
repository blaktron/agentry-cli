// Plan-type tests: hash stability and the version gate.
package spec

import (
	"strings"
	"testing"
)

func samplePlan() *Plan {
	return &Plan{
		PlanVersion: PlanVersion,
		Ref:         "demo/x@1.0.0",
		Digest:      "sha256:abc",
		Source:      "local",
		Kind:        "skill",
		Enforcement: map[string]string{"network": "none", "exec": "none"},
		Tools:       []string{"write_report"},
		Files:       map[string]string{"gateway/catalog.yaml": "aa"},
	}
}

func TestHashStableAndSensitive(t *testing.T) {
	p := samplePlan()
	h1, err := p.Hash()
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := samplePlan().Hash()
	if h1 != h2 {
		t.Fatalf("identical plans hashed differently: %s vs %s", h1, h2)
	}
	p.Tools = append(p.Tools, "fetch")
	h3, _ := p.Hash()
	if h3 == h1 {
		t.Fatal("changing the plan did not change the hash")
	}
}

func TestParsePlanVersionGate(t *testing.T) {
	b, err := samplePlan().MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePlan(b); err != nil {
		t.Fatalf("current version rejected: %v", err)
	}
	old := strings.Replace(string(b), `"planVersion": "1"`, `"planVersion": "0"`, 1)
	if _, err := ParsePlan([]byte(old)); err == nil {
		t.Fatal("foreign plan version accepted")
	}
}

func TestPlaceholdersDoNotCollideWithGatewayTemplates(t *testing.T) {
	// The gateway templates {{server.key}} and docker-agent expands
	// ${env.VAR}; our placeholders must never look like either.
	for _, ph := range []string{PHWorkspace, PHOut, PHCfg, PHSecrets, PHRunID} {
		if strings.Contains(ph, "{{") || strings.Contains(ph, "${") {
			t.Errorf("placeholder %q collides with downstream template syntax", ph)
		}
		if !strings.HasPrefix(ph, "@@") || !strings.HasSuffix(ph, "@@") {
			t.Errorf("placeholder %q must keep the @@…@@ shape", ph)
		}
	}
}

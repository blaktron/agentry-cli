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
		Modes:       []string{"autonomous"},
		Mode:        "autonomous",
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

// TestEndpointURLs pins the exact on-network names: the compiler bakes
// these strings into gateway files and harness definitions, so a change
// here is a breaking change to every installed plan.
func TestEndpointURLs(t *testing.T) {
	if got := GatewayURL(); got != "http://gateway:8811/mcp" {
		t.Errorf("GatewayURL = %q", got)
	}
	if got := ProxyURL(); got != "http://llm-proxy:3128" {
		t.Errorf("ProxyURL = %q", got)
	}
}

func TestParsePlanInvalidJSON(t *testing.T) {
	if _, err := ParsePlan([]byte("{not json")); err == nil || !strings.Contains(err.Error(), "parse plan.json") {
		t.Fatalf("err = %v", err)
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
	old := strings.Replace(string(b), `"planVersion": "`+PlanVersion+`"`, `"planVersion": "0"`, 1)
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

// TestExecImageTagMovesWithTheCatalog (agentry-cli#100): the exec image
// is named by the runner image, the commands, the catalogued packages and
// the mirror, so a changed catalog entry or mirror is a new image; the
// same inputs in another order name the same image.
func TestExecImageTagMovesWithTheCatalog(t *testing.T) {
	a := ExecImageTag("agentry-runner:v1", []string{"git", "jq"}, []string{"git", "jq"}, "")
	if a == "" || !strings.HasPrefix(a, "agentry-runner-exec:v1-") {
		t.Fatalf("tag = %q", a)
	}
	if b := ExecImageTag("agentry-runner:v1", []string{"jq", "git"}, []string{"jq", "git"}, ""); b != a {
		t.Fatalf("order changed the tag: %q vs %q", a, b)
	}
	if b := ExecImageTag("agentry-runner:v1", []string{"git", "jq"}, []string{"git=2.49.1-r0", "jq"}, ""); b == a {
		t.Fatal("a pinned package did not change the tag")
	}
	if b := ExecImageTag("agentry-runner:v1", []string{"git", "jq"}, []string{"git", "jq"}, "https://mirror.example/alpine"); b == a {
		t.Fatal("a mirror did not change the tag")
	}
	if ExecImageTag("agentry-runner:v1", nil, nil, "") != "" {
		t.Fatal("no commands must mean no exec image")
	}
}

// An Unsafe Mode exec image's tag covers the base (pinned by digest), the
// runner image, the sorted commands, the resolved packages and the extras
// (plan unsafe-mode.md invariant 9), so a re-pin or a changed index is a
// new image; it stays in the runner's -exec repository, where the
// desktop's image removal finds it, and never equals a catalogue tag.
func TestUnsafeExecImageTagCoversWhatBuildsIt(t *testing.T) {
	base := "ghcr.io/blaktron/agentry-unsafe-kali:2026.09.30.2@sha256:87fa4ae406a9621cef3a4cffcdccf2a512da03141ca21727ce663ca715a191da"
	u := UnsafeExecSpec{Base: base, Extras: []UnsafeExtra{{Command: "decompyle3", Kind: "pip", Spec: "decompyle3==3.9.3"}}}
	cmds := []string{"decompyle3", "file", "r2"}
	pkgs := []string{"file", "radare2"}
	a := UnsafeExecImageTag("agentry-runner:v1", u, cmds, pkgs)
	if !strings.HasPrefix(a, "agentry-runner-exec:v1-u") {
		t.Fatalf("tag = %q, want the runner's -exec repository and a u tag", a)
	}
	if a == ExecImageTag("agentry-runner:v1", cmds, pkgs, "") {
		t.Fatal("an unsafe tag equals the catalogue's for the same commands")
	}
	if b := UnsafeExecImageTag("agentry-runner:v1", u, []string{"r2", "file", "decompyle3"}, []string{"radare2", "file"}); b != a {
		t.Fatalf("order changed the tag: %q vs %q", a, b)
	}
	changed := map[string]string{}
	repinned := u
	repinned.Base = strings.Replace(base, "87fa4ae4", "00000000", 1)
	changed["the base digest"] = UnsafeExecImageTag("agentry-runner:v1", repinned, cmds, pkgs)
	changed["the runner image"] = UnsafeExecImageTag("agentry-runner:v2", u, cmds, pkgs)
	changed["the commands"] = UnsafeExecImageTag("agentry-runner:v1", u, append(cmds, "xxd"), pkgs)
	changed["the packages"] = UnsafeExecImageTag("agentry-runner:v1", u, cmds, append(pkgs, "xxd"))
	other := u
	other.Extras = []UnsafeExtra{{Command: "decompyle3", Kind: "pip", Spec: "decompyle3==3.9.4"}}
	changed["an extra's version"] = UnsafeExecImageTag("agentry-runner:v1", other, cmds, pkgs)
	for what, b := range changed {
		if b == a {
			t.Errorf("%s did not change the tag", what)
		}
	}
	if UnsafeExecImageTag("agentry-runner:v1", u, nil, nil) != "" {
		t.Fatal("no commands must mean no exec image")
	}
}

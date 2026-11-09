package drafter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
	"github.com/blaktron/agentry/pkg/toolgrant"
)

// A draft that declares a server for one of the library's applications is
// refused, naming the line to declare instead; a draft with the desktop
// line is not; the grants' lines send the repair turn to the guide's page.
func TestADraftUsesThePersonsComputerThroughTheDesktopLine(t *testing.T) {
	bad := json.RawMessage(`{"schemaVersion":"2","name":"scene","namespace":"acme","version":"1.0.0","modes":["autonomous"],
		"capabilities":{"network":{"egress":"none"},"mcpServers":[{"pypi":"mcp-for-blender@2.2.7"}]}}`)
	probs := desktopServerProblems(bad)
	if len(probs) != 1 || !strings.Contains(probs[0], `capabilities.desktop.servers: [{"id": "blender"}]`) || !strings.Contains(probs[0], "Blender") {
		t.Fatalf("a draft declaring Blender's server: %q", probs)
	}
	good := json.RawMessage(`{"schemaVersion":"2","name":"scene","namespace":"acme","version":"1.0.0","modes":["autonomous"],
		"capabilities":{"network":{"egress":"none"},"desktop":{"servers":[{"id":"blender"}],"computerUse":"app"}}}`)
	if probs := desktopServerProblems(good); len(probs) != 0 {
		t.Fatalf("a draft with the desktop line: %q", probs)
	}
	for _, g := range []toolgrant.Grant{toolgrant.GrantDesktop, toolgrant.GrantComputerUse} {
		if got := grantGuide(g); got != "manifest/desktop.md" {
			t.Errorf("%s's guide page is %s", g, got)
		}
	}
	if string(guideFile(t, "manifest/desktop.md")) == "" {
		t.Fatal("the guide has no page on the person's computer")
	}
}

// The guide's example line, which names a library preset, an application
// that is not one used through its windows, and an application the person
// chooses, is a line the contract takes, and a draft carrying it raises no
// problem of the Drafter's.
func TestTheGuidesDesktopLineIsValid(t *testing.T) {
	page := string(guideFile(t, "manifest/desktop.md"))
	start := strings.Index(page, "```json\n")
	end := strings.Index(page[start+8:], "```")
	if start < 0 || end < 0 {
		t.Fatal("the guide's page has no example line")
	}
	var caps struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte("{"+page[start+8:start+8+end]+"}"), &caps); err != nil {
		t.Fatalf("the example line: %v", err)
	}
	for _, id := range []string{`"id": "blender"`, `"id": "kdenlive", "use": "computer"`, `"choose": true`} {
		if !strings.Contains(string(caps.Capabilities), id) {
			t.Errorf("the example line lacks %s", id)
		}
	}
	raw := json.RawMessage(`{"schemaVersion":"2","name":"scene","namespace":"acme","version":"1.0.0","modes":["autonomous"],
		"capabilities":` + string(caps.Capabilities) + `}`)
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range manifest.Validate(m) {
		if strings.Contains(err.Error(), "desktop") {
			t.Errorf("the contract refuses the guide's line: %v", err)
		}
	}
	if probs := desktopServerProblems(raw); len(probs) != 0 {
		t.Errorf("the Drafter refuses the guide's line: %q", probs)
	}
	mycad := json.RawMessage(`{"schemaVersion":"2","name":"cad","namespace":"acme","version":"1.0.0","modes":["autonomous"],
		"capabilities":{"network":{"egress":"none"},"desktop":{"servers":[{"id":"my-cad","tools":["export_drawing"]}]}}}`)
	if m, err = manifest.Parse(mycad); err != nil {
		t.Fatal(err)
	}
	for _, err := range manifest.Validate(m) {
		if strings.Contains(err.Error(), "desktop") {
			t.Errorf("the contract refuses an application that is not a preset: %v", err)
		}
	}
}

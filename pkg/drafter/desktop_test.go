package drafter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/toolgrant"
)

// A draft that declares a server for an application of the catalogue is
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

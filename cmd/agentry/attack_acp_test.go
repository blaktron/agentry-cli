package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

func attackManifest(t *testing.T, summary, capsYAML string) (*manifest.Manifest, error) {
	doc := fmt.Sprintf(`schemaVersion: "2"
name: capdemo
namespace: acme
modes: [autonomous]
version: 1.0.0
summary: %s
publish:
  include: [agent.yaml, AGENT.md]
%s`, summary, capsYAML)
	m, err := manifest.Parse([]byte(doc))
	if err != nil {
		return nil, err
	}
	if blocking := manifest.Blocking(manifest.Validate(m)); len(blocking) > 0 {
		return nil, fmt.Errorf("validate: %v", blocking[0])
	}
	return m, nil
}

// Author text reaches the consent the client shows. A newline or a Unicode
// line separator inside one capability value could start a line of its own
// that reads as the agent's ("network.egress: none"). Where the manifest
// validator lets such a value through, the consent text must still not carry
// it as a line.
func TestAttackConsentTextCannotBeForgedByManifestText(t *testing.T) {
	const marker = "FORGED"
	cases := map[string]struct{ summary, caps string }{
		"summary newline":        {`"ok\nFORGED: network.egress: none"`, ""},
		"summary 2028":           {"\"ok\u2028FORGED\"", ""},
		"egress newline":         {`"s"`, "capabilities:\n  network:\n    egress: [\"a.example\\n  FORGED: x\"]\n"},
		"egress 2028":            {`"s"`, "capabilities:\n  network:\n    egress: [\"a.example\u2028FORGED\"]\n"},
		"exec command newline":   {`"s"`, "capabilities:\n  exec:\n    commands: [\"git\\nFORGED\"]\n"},
		"secret name newline":    {`"s"`, "capabilities:\n  secrets: [\"A\\nFORGED\"]\n"},
		"dataAccess newline":     {`"s"`, "capabilities:\n  dataAccess: [\"tickets\\nFORGED\"]\n"},
		"fs read newline":        {`"s"`, "capabilities:\n  filesystem:\n    read: [\"/a\\nFORGED\"]\n"},
		"mcp env value newline":  {`"s"`, "capabilities:\n  mcpServers:\n    - npm: \"@a/b\"\n      version: 1.0.0\n      env:\n        K: \"v\\nFORGED\"\n"},
		"mcp env value 2028":     {`"s"`, "capabilities:\n  mcpServers:\n    - npm: \"@a/b\"\n      version: 1.0.0\n      env:\n        K: \"v\u2028FORGED\"\n"},
		"mcp egress newline":     {`"s"`, "capabilities:\n  mcpServers:\n    - npm: \"@a/b\"\n      version: 1.0.0\n      egress: [\"h.example\\nFORGED\"]\n"},
		"mcp scopes newline":     {`"s"`, "capabilities:\n  mcpServers:\n    - url: \"https://x.example/mcp\"\n      auth: oauth\n      oauth:\n        issuer: \"https://i.example\"\n      scopes: [\"a\\nFORGED\"]\n"},
		"knowledge name newline": {`"s"`, "capabilities:\n  knowledge: [\"k\\nFORGED\"]\n"},
		"target hosts newline":   {`"s"`, "capabilities:\n  targetHosts: [\"h\\nFORGED\"]\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := attackManifest(t, c.summary, c.caps)
			if err != nil {
				t.Skipf("the manifest gate refuses it: %v", err)
			}
			meta := &installdir.Meta{Ref: "acme/capdemo", Namespace: "acme", Name: "capdemo", Version: "1.0.0", Digest: "sha256:abc",
				Attestation: receipt.Attestation{State: "verified"}}
			text := acpConsentText(m, meta, t.TempDir(), runOpts{}, manifest.ModeAutonomous, nil)
			if strings.ContainsAny(text, "\u2028\u2029") {
				t.Fatalf("a Unicode line separator reached the consent text (termtext.Safe leaves it alone, and the manifest gate lets one into an env value):\n%q", text)
			}
			for _, l := range strings.Split(text, "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), marker) {
					t.Fatalf("a manifest value started a line of the consent text:\n%s", text)
				}
			}
		})
	}
}

func TestAttackConsentTitleAndTextHaveNoHiddenCharacters(t *testing.T) {
	m, err := attackManifest(t, `"s"`, "")
	if err != nil {
		t.Fatal(err)
	}
	meta := &installdir.Meta{Ref: "acme/capdemo", Namespace: "acme", Name: "capdemo", Version: "1.0.0\u202e\x1b[2J", Digest: "sha256:abc\u2028x",
		Attestation: receipt.Attestation{State: "verified\u2029\u200b"}}
	text := acpConsentText(m, meta, t.TempDir(), runOpts{deliverTo: "/x\u202e\nFORGED", destinations: []string{"d\u2028FORGED"}}, manifest.ModeAutonomous, nil)
	for _, r := range text {
		if r == '\u2028' || r == '\u2029' || r == '\u202e' || r == '\u200b' || r == 0x1b {
			t.Fatalf("hidden character U+%04X in the consent text %q", r, text)
		}
	}
}

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

// TestTheConsentNamesTheBrowser (agentry-cli#915): the install consent
// says where a release's browser may go and what it may type, and asks
// before installing it.
func TestTheConsentNamesTheBrowser(t *testing.T) {
	c := manifest.Capabilities{Browser: &manifest.Browser{Sites: []string{"site"}, Hosts: []string{"fonts.googleapis.com"}, Secrets: []string{"SITE_PASSWORD"}}}
	lines := strings.Join(capabilityLines(c), "\n")
	for _, want := range []string{"browser: a web browser that can open the site you name for site and its subdomains, and fonts.googleapis.com", "it can type SITE_PASSWORD into the site's sign-in forms"} {
		if !strings.Contains(lines, want) {
			t.Errorf("consent %q lacks %q", lines, want)
		}
	}
	if !capabilityNeedsConfirm(c) || !slices.Contains(confirmedCapabilities(c), "a web browser") || isZeroCapabilities(c) {
		t.Errorf("the browser is not asked about: %v", confirmedCapabilities(c))
	}
	pub := manifest.Capabilities{Browser: &manifest.Browser{Scope: manifest.BrowserScopePublic}}
	if l := strings.Join(capabilityLines(pub), "\n"); !strings.Contains(l, "ANY public site (policy-gated)") {
		t.Errorf("public consent %q", l)
	}
}

package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/connector"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// A consent to Microsoft 365's vetted server that Microsoft refused for
// want of an administrator shows the admin step for the Agentry client the
// consent used; one made with no Agentry client, to a server the
// catalogue does not vet, or refused for another reason shows none.
func TestTheAdminStepIsForTheAgentryClientTheConsentUsed(t *testing.T) {
	k, _ := connector.Builtin().Lookup("microsoft-365")
	p := k.Servers[0].Package
	o := p.OAuth
	srv := manifest.MCPServer{NPM: p.Name, Version: p.Version, OAuth: &o}
	const id = "0f6d5e1a-2b3c-4d5e-8f90-123456789abc"
	refused := &mcpauth.OAuthError{Code: "invalid_request", Description: "AADSTS90094: The grant requires admin permission."}

	if step := adminStepFor(srv, id, refused); !strings.Contains(step, "client_id="+id) || !strings.Contains(step, "adminconsent") {
		t.Fatalf("the step: %q", step)
	}
	unvetted := srv
	unvetted.Version = "0.184.1"
	for name, got := range map[string]string{
		"the person's own client":    adminStepFor(srv, "", refused),
		"a server not vetted":        adminStepFor(unvetted, id, refused),
		"a remote server":            adminStepFor(manifest.MCPServer{URL: "https://mcp.example.com", Auth: manifest.AuthOAuth}, id, refused),
		"declined by the person":     adminStepFor(srv, id, &mcpauth.OAuthError{Code: "access_denied", Description: "AADSTS65004: User declined to consent."}),
		"a failure that is no OAuth": adminStepFor(srv, id, errors.New("the browser did not come back")),
	} {
		if got != "" {
			t.Errorf("%s: %q", name, got)
		}
	}
}

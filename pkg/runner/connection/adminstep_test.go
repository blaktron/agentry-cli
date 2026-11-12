package connection

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/connector"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

const msClient = "0f6d5e1a-2b3c-4d5e-8f90-123456789abc"

// msServer is the first Microsoft 365 server the built-in catalogue vets,
// declared as a release would declare it.
func msServer(t *testing.T) (manifest.MCPServer, connector.Connector) {
	t.Helper()
	k, ok := connector.Builtin().Lookup("microsoft-365")
	if !ok || len(k.Servers) == 0 || k.Servers[0].Package == nil {
		t.Fatalf("the catalogue vets no Microsoft 365 package: %+v", k)
	}
	p := k.Servers[0].Package
	o := p.OAuth
	return manifest.MCPServer{NPM: p.Name, Version: p.Version, OAuth: &o}, k
}

// A consent Microsoft refused because an administrator must approve the
// app gets the step with the tenant-wide admin-consent link for the client
// it used and the scopes it asked for; any other refusal, any other error,
// and a connector with no admin step get nothing.
func TestARefusalThatNeedsAnAdminGetsTheAdminLink(t *testing.T) {
	srv, k := msServer(t)
	reg := newFakeRegistry(t, map[string]string{"id": "microsoft-365", "publicClientId": msClient})
	p := OpenPublicClients(t.TempDir(), reg.srv.URL, http.DefaultClient)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	got, id, ok := p.ConnectorFor(context.Background(), srv, srv.OAuth.Issuer)
	if !ok || got.ID != "microsoft-365" || id != msClient {
		t.Fatalf("ConnectorFor: %+v %q %v", got, id, ok)
	}
	refused := &mcpauth.OAuthError{Code: "invalid_request", Description: "AADSTS90094: The grant requires admin permission."}
	step := AdminStep(refused, got, id, srv.OAuth.Scopes)
	for _, want := range []string{"administrator", "Microsoft 365", "https://login.microsoftonline.com/organizations/v2.0/adminconsent?", "client_id=" + msClient, "Mail.Read"} {
		if !strings.Contains(step, want) {
			t.Fatalf("the step lacks %q:\n%s", want, step)
		}
	}
	if s := AdminStep(errors.Join(errors.New("connect"), refused), got, id, srv.OAuth.Scopes); s == "" {
		t.Error("a wrapped refusal got no step")
	}
	for name, err := range map[string]error{
		"declined by the person": &mcpauth.OAuthError{Code: "access_denied", Description: "AADSTS65004: User declined to consent."},
		"another failure":        errors.New("the browser did not come back"),
		"no error":               nil,
	} {
		if s := AdminStep(err, got, id, srv.OAuth.Scopes); s != "" {
			t.Errorf("%s: %s", name, s)
		}
	}
	gh, _ := connector.Builtin().Lookup("github")
	if s := AdminStep(refused, gh, id, nil); s != "" {
		t.Errorf("a connector with no admin step: %s", s)
	}
	if s := AdminStep(refused, k, "not-an-entra-id", srv.OAuth.Scopes); s != "" {
		t.Errorf("a client id the link cannot carry: %s", s)
	}
}

// A runhost making a connection knows the issuer and the client the device
// gave it: Entra's common authority finds Microsoft 365, with or without a
// trailing slash, and no other issuer finds a connector with an admin step.
func TestConnectorAtIssuer(t *testing.T) {
	for _, iss := range []string{"https://login.microsoftonline.com/common/v2.0", "https://login.microsoftonline.com/common/v2.0/"} {
		if k, ok := ConnectorAtIssuer(iss); !ok || k.ID != "microsoft-365" {
			t.Errorf("%s: %+v %v", iss, k, ok)
		}
	}
	for _, iss := range []string{"https://login.microsoftonline.com/organizations/v2.0", "https://github.com/login/oauth", "https://app.asana.com", ""} {
		if k, ok := ConnectorAtIssuer(iss); ok {
			t.Errorf("%s found %s", iss, k.ID)
		}
	}
}

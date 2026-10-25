package connection

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/connector"
	"github.com/blaktron/agentry/pkg/manifest"
)

const testCatalogue = `
connectors:
  - id: example-mail
    vendor: Example
    name: Example Mail
    strategy: agentry-public
    issuer: https://login.example.com
    servers:
      - remote: https://mcp.example.com/mail
  - id: example-files
    vendor: Example
    name: Example Files
    strategy: agentry-confidential
    issuer: https://login.example.com
    servers:
      - remote: https://mcp.example.com/files
`

// fakeRegistry answers GET /api/v1/connectors with answer, counting asks;
// down makes it answer 503.
type fakeRegistry struct {
	srv    *httptest.Server
	asks   atomic.Int32
	down   atomic.Bool
	answer []map[string]string
}

func newFakeRegistry(t *testing.T, answer ...map[string]string) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{answer: answer}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/connectors" || r.Header.Get("Authorization") != "" {
			t.Errorf("asked %s with authorization %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		f.asks.Add(1)
		if f.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"connectors": f.answer})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func testPublicClients(t *testing.T, home, registry string, now *time.Time) *PublicClients {
	t.Helper()
	c, err := connector.Parse([]byte(testCatalogue))
	if err != nil {
		t.Fatal(err)
	}
	p := OpenPublicClients(home, registry, http.DefaultClient)
	p.catalogue = c
	p.now = func() time.Time { return *now }
	return p
}

func mail() manifest.MCPServer {
	return manifest.MCPServer{URL: "https://mcp.example.com/mail", Auth: manifest.AuthOAuth}
}

const issuer = "https://login.example.com"

// A server the catalogue vets for a public-client connector gets the id
// the registry serves for it, asked once and then kept.
func TestAVettedServerGetsTheRegistrysPublicClient(t *testing.T) {
	reg := newFakeRegistry(t, map[string]string{"id": "example-mail", "publicClientId": "pub-123"})
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	p := testPublicClients(t, t.TempDir(), reg.srv.URL, &now)
	for range 3 {
		if got := p.ForServer(context.Background(), mail(), issuer); got != "pub-123" {
			t.Fatalf("got %q", got)
		}
	}
	if reg.asks.Load() != 1 {
		t.Fatalf("the registry was asked %d times, want once", reg.asks.Load())
	}
}

// A server the catalogue does not vet, the same server at another issuer,
// and a confidential connector's server get no Agentry client, whatever the
// registry serves; neither does a vetted server with no id served.
func TestNoAgentryClientForWhatTheCatalogueDoesNotVet(t *testing.T) {
	reg := newFakeRegistry(t,
		map[string]string{"id": "example-mail", "publicClientId": "pub-123"},
		map[string]string{"id": "example-files", "publicClientId": "should-not-be-used"},
	)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	p := testPublicClients(t, t.TempDir(), reg.srv.URL, &now)
	for name, c := range map[string]struct {
		srv    manifest.MCPServer
		issuer string
	}{
		"another address":       {manifest.MCPServer{URL: "https://mcp.example.com/mail-fork", Auth: manifest.AuthOAuth}, issuer},
		"a climbing path":       {manifest.MCPServer{URL: "https://mcp.example.com/mail/../files", Auth: manifest.AuthOAuth}, issuer},
		"another issuer":        {mail(), "https://login.example.org"},
		"a confidential app":    {manifest.MCPServer{URL: "https://mcp.example.com/files", Auth: manifest.AuthOAuth}, issuer},
		"a package of its name": {manifest.MCPServer{NPM: "mcp-example-mail", Version: "1.0.0", OAuth: &manifest.MCPOAuth{Issuer: issuer}}, issuer},
	} {
		if got := p.ForServer(context.Background(), c.srv, c.issuer); got != "" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	none := newFakeRegistry(t, map[string]string{"id": "example-mail"})
	q := testPublicClients(t, t.TempDir(), none.srv.URL, &now)
	if got := q.ForServer(context.Background(), mail(), issuer); got != "" {
		t.Errorf("with no id served: got %q", got)
	}
}

// An id the registry serves for a connector this build does not know, for a
// confidential one, or not in a client id's shape is not taken.
func TestOnlyWellFormedIDsForKnownPublicConnectorsAreTaken(t *testing.T) {
	reg := newFakeRegistry(t,
		map[string]string{"id": "example-mail", "publicClientId": "two words"},
		map[string]string{"id": "nobody", "publicClientId": "pub-1"},
	)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	p := testPublicClients(t, t.TempDir(), reg.srv.URL, &now)
	got := p.ids(context.Background())
	if len(got) != 0 {
		t.Fatalf("taken: %v", got)
	}
}

// The kept answer is asked again once a day old; a registry that cannot be
// reached leaves the kept answer in use, and with none kept there is none.
func TestTheKeptAnswerIsRenewedDailyAndUsedOffline(t *testing.T) {
	home := t.TempDir()
	reg := newFakeRegistry(t, map[string]string{"id": "example-mail", "publicClientId": "pub-1"})
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	p := testPublicClients(t, home, reg.srv.URL, &now)
	if got := p.ForServer(context.Background(), mail(), issuer); got != "pub-1" {
		t.Fatalf("first: %q", got)
	}
	reg.answer = []map[string]string{{"id": "example-mail", "publicClientId": "pub-2"}}
	now = now.Add(PublicClientsTTL - time.Minute)
	if got := p.ForServer(context.Background(), mail(), issuer); got != "pub-1" || reg.asks.Load() != 1 {
		t.Fatalf("within a day: %q, asked %d", got, reg.asks.Load())
	}
	now = now.Add(2 * time.Minute)
	if got := p.ForServer(context.Background(), mail(), issuer); got != "pub-2" || reg.asks.Load() != 2 {
		t.Fatalf("after a day: %q, asked %d", got, reg.asks.Load())
	}
	reg.down.Store(true)
	now = now.Add(30 * 24 * time.Hour)
	if got := p.ForServer(context.Background(), mail(), issuer); got != "pub-2" {
		t.Fatalf("offline with a kept answer: %q", got)
	}
	fresh := testPublicClients(t, t.TempDir(), reg.srv.URL, &now)
	if got := fresh.ForServer(context.Background(), mail(), issuer); got != "" {
		t.Fatalf("offline with nothing kept: %q", got)
	}
}

// One registry's kept answer is never another's.
func TestTheKeptAnswerBelongsToItsRegistry(t *testing.T) {
	home := t.TempDir()
	a := newFakeRegistry(t, map[string]string{"id": "example-mail", "publicClientId": "pub-a"})
	b := newFakeRegistry(t)
	b.down.Store(true)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if got := testPublicClients(t, home, a.srv.URL, &now).ForServer(context.Background(), mail(), issuer); got != "pub-a" {
		t.Fatalf("a: %q", got)
	}
	if got := testPublicClients(t, home, b.srv.URL, &now).ForServer(context.Background(), mail(), issuer); got != "" {
		t.Fatalf("b read a's answer: %q", got)
	}
}

// A nil PublicClients (a caller with no registry) gives no client.
func TestNilPublicClientsGiveNone(t *testing.T) {
	var p *PublicClients
	if got := p.ForServer(context.Background(), mail(), issuer); got != "" {
		t.Fatalf("got %q", got)
	}
}

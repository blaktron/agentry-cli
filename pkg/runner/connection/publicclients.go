package connection

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/connector"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/internal/storefile"
)

// PublicClientsTTL is how long the registry's answer is used before it is
// asked again.
const PublicClientsTTL = 24 * time.Hour

// publicClientsTimeout bounds asking the registry, so a registry that does
// not answer delays a sign-in by seconds, not minutes.
const publicClientsTimeout = 10 * time.Second

// PublicClients is the client ids of Agentry's own public clients, which
// the registry serves for its connectors (GET /api/v1/connectors). A
// sign-in uses one only for a server the connector catalogue this build
// carries vets for that connector, signed in to at the connector's issuer.
// The answer is kept per registry under the runner home and asked again
// once it is a day old; a registry that cannot be reached leaves the kept
// answer in use, however old, and with none kept the sign-in goes on
// without Agentry's client.
type PublicClients struct {
	dir       string
	registry  string
	http      *http.Client
	now       func() time.Time
	catalogue connector.Catalogue
}

// OpenPublicClients is the registry's public client ids, kept under home.
func OpenPublicClients(home, registry string, c *http.Client) *PublicClients {
	return &PublicClients{
		dir: filepath.Join(home, "connectors"), registry: strings.TrimRight(registry, "/"),
		http: c, now: time.Now, catalogue: connector.Builtin(),
	}
}

// keptPublicClients is the file kept for one registry.
type keptPublicClients struct {
	Registry      string            `json:"registry"`
	FetchedAt     time.Time         `json:"fetchedAt"`
	PublicClients map[string]string `json:"publicClients"`
}

// ForServer is the client id of Agentry's own public client for a
// sign-in to server at issuer, or "": the catalogue must vet the server
// for a connector whose app is a public client, and the registry must
// serve that connector's id.
func (p *PublicClients) ForServer(ctx context.Context, server manifest.MCPServer, issuer string) string {
	if p == nil {
		return ""
	}
	k, ok := p.catalogue.Match(server, issuer)
	if !ok || !k.HasPublicClient() {
		return ""
	}
	return p.ids(ctx)[k.ID]
}

// ids is the registry's answer: the kept one while it is fresh, else a new
// one, else the kept one however old, else none.
func (p *PublicClients) ids(ctx context.Context) map[string]string {
	kept, _ := p.load()
	if kept != nil && p.now().Sub(kept.FetchedAt) < PublicClientsTTL {
		return kept.PublicClients
	}
	fresh, err := p.fetch(ctx)
	if err != nil {
		if kept != nil {
			return kept.PublicClients
		}
		return nil
	}
	_ = p.save(&keptPublicClients{Registry: p.registry, FetchedAt: p.now().UTC(), PublicClients: fresh})
	return fresh
}

func (p *PublicClients) path() string {
	u, err := url.Parse(p.registry)
	host := p.registry
	if err == nil && u.Host != "" {
		host = u.Host
	}
	return filepath.Join(p.dir, strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, host)+".json")
}

// load is the kept answer for this registry, or nil.
func (p *PublicClients) load() (*keptPublicClients, error) {
	b, err := os.ReadFile(p.path())
	if err != nil {
		return nil, err
	}
	var k keptPublicClients
	if err := json.Unmarshal(b, &k); err != nil || k.Registry != p.registry {
		return nil, fmt.Errorf("the kept public client ids are not this registry's")
	}
	return &k, nil
}

func (p *PublicClients) save(k *keptPublicClients) error {
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return storefile.WriteAtomic(p.path(), append(b, '\n'))
}

// publicClientRE is a client id as vendors issue them: one word, at most
// 256 characters.
var publicClientRE = regexp.MustCompile(`^[A-Za-z0-9._~:/@+=-]{1,256}$`)

// fetch asks the registry, without a credential, for its connectors' public
// client ids. An id is taken only for a connector this build's catalogue
// knows as a public client, and only in a client id's shape.
func (p *PublicClients) fetch(ctx context.Context) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, publicClientsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.registry+"/api/v1/connectors", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the registry's connectors answered %s", resp.Status)
	}
	var doc struct {
		Connectors []struct {
			ID             string `json:"id"`
			PublicClientID string `json:"publicClientId"`
		} `json:"connectors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("read the registry's connectors: %w", err)
	}
	out := map[string]string{}
	for _, c := range doc.Connectors {
		k, ok := p.catalogue.Lookup(c.ID)
		if ok && k.HasPublicClient() && publicClientRE.MatchString(c.PublicClientID) {
			out[c.ID] = c.PublicClientID
		}
	}
	return out, nil
}

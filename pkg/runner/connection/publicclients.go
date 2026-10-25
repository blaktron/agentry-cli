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

// publicClientsRetry is how long a registry that could not be asked is
// left alone, so a registry that is down does not delay every sign-in.
const publicClientsRetry = 10 * time.Minute

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
	Registry  string    `json:"registry"`
	FetchedAt time.Time `json:"fetchedAt"`
	// TriedAt is the last time the registry could not be asked.
	TriedAt       time.Time         `json:"triedAt,omitempty"`
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

// ForConnector is the client id the registry serves for the connector id,
// or "", without matching a declared server: for a server Agentry itself
// built and offers (an agent's own image), whose caller vouches for it.
func (p *PublicClients) ForConnector(ctx context.Context, id string) string {
	if p == nil {
		return ""
	}
	if k, ok := p.catalogue.Lookup(id); !ok || !k.HasPublicClient() {
		return ""
	}
	return p.ids(ctx)[id]
}

// ids is the registry's answer: the kept one while it is fresh, else a new
// one, else the kept one however old, else none.
func (p *PublicClients) ids(ctx context.Context) map[string]string {
	kept, _ := p.load()
	now := p.now()
	fresh := func(at time.Time, d time.Duration) bool { return !at.After(now) && now.Sub(at) < d }
	if kept != nil && (fresh(kept.FetchedAt, PublicClientsTTL) || fresh(kept.TriedAt, publicClientsRetry)) {
		return kept.PublicClients
	}
	got, err := p.fetch(ctx)
	if err != nil {
		if kept == nil {
			kept = &keptPublicClients{Registry: p.registry}
		}
		kept.TriedAt = now.UTC()
		_ = p.save(kept)
		return kept.PublicClients
	}
	_ = p.save(&keptPublicClients{Registry: p.registry, FetchedAt: now.UTC(), PublicClients: got})
	return got
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
	k.PublicClients = p.taken(k.PublicClients)
	return &k, nil
}

func (p *PublicClients) save(k *keptPublicClients) error {
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return storefile.WriteAtomic(p.path(), append(b, '\n'))
}

// publicClientRE is a public client id as vendors issue them (a GUID at
// Microsoft): letters, digits, dots, hyphens and underscores, at most 256.
// It is narrower than what the registry's configuration accepts, and
// refuses a URL, which a vendor taking client metadata documents would
// read as one.
var publicClientRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)

// taken is ids reduced to those for a connector this build's catalogue
// knows as a public client, each in a client id's shape.
func (p *PublicClients) taken(ids map[string]string) map[string]string {
	out := map[string]string{}
	for id, client := range ids {
		k, ok := p.catalogue.Lookup(id)
		if ok && k.HasPublicClient() && publicClientRE.MatchString(client) {
			out[id] = client
		}
	}
	return out
}

// askable reports whether the registry's address may be asked for client
// ids: https, or plain http to the loopback (a registry run on this
// machine), so no one on the network between can name the client.
func askable(registry string) bool {
	u, err := url.Parse(registry)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	h := u.Hostname()
	return u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1")
}

// fetch asks the registry, without a credential, for its connectors' public
// client ids. An id is taken only for a connector this build's catalogue
// knows as a public client, and only in a client id's shape.
func (p *PublicClients) fetch(ctx context.Context) (map[string]string, error) {
	if !askable(p.registry) {
		return nil, fmt.Errorf("the registry %s is not reached over https, so its client ids are not taken", p.registry)
	}
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
	ids := map[string]string{}
	for _, c := range doc.Connectors {
		ids[c.ID] = c.PublicClientID
	}
	return p.taken(ids), nil
}

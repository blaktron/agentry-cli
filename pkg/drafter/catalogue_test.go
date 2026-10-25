package drafter

import (
	"fmt"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/connector"
	"github.com/blaktron/agentry/pkg/manifest"
)

// catalogueDisagreements is every entry of the guide's lists that a
// connector with Agentry's own public client vets but that tells a draft
// the person brings their own client.
func catalogueDisagreements(l *ServerList, c connector.Catalogue) []string {
	var out []string
	for _, k := range c.All() {
		if !k.HasPublicClient() {
			continue
		}
		for _, r := range l.Remote {
			if r.Auth == manifest.AuthOAuth && k.Vets(manifest.MCPServer{URL: r.URL, Auth: r.Auth}) && r.Client != ClientAgentry {
				out = append(out, fmt.Sprintf("remote %s: %s vets it with Agentry's own client, the list says %q", r.URL, k.ID, r.Client))
			}
		}
		for _, si := range l.SignIns {
			oauth := si.OAuth
			srv := manifest.MCPServer{Version: si.Versions, OAuth: &oauth}
			if si.Ecosystem == "pypi" {
				srv.PyPI = si.Package
			} else {
				srv.NPM = si.Package
			}
			if k.Vets(srv) && si.Client != ClientAgentry {
				out = append(out, fmt.Sprintf("sign-in %s %s: %s vets it with Agentry's own client, the list says %q", si.Package, si.Versions, k.ID, si.Client))
			}
		}
	}
	return out
}

// The guide's lists agree with the connector catalogue this build carries:
// a server a connector vets with Agentry's own public client is listed as
// signing in with Agentry's client, so a draft does not send the person to
// make a client of their own.
func TestTheGuideAgreesWithTheConnectorCatalogue(t *testing.T) {
	l, err := LoadServerList(FS())
	if err != nil {
		t.Fatal(err)
	}
	if bad := catalogueDisagreements(l, connector.Builtin()); len(bad) > 0 {
		t.Fatalf("the drafting guide's servers.yaml disagrees with the connector catalogue:\n%s", strings.Join(bad, "\n"))
	}
}

// The check finds an entry a public-client connector vets that the list
// says needs the person's own client.
func TestTheCatalogueCheckFindsADisagreement(t *testing.T) {
	l, err := LoadServerList(FS())
	if err != nil {
		t.Fatal(err)
	}
	c, err := connector.Parse([]byte(`
connectors:
  - id: asana-public
    vendor: Asana
    name: Asana
    strategy: agentry-public
    issuer: https://app.asana.com
    servers:
      - remote: https://mcp.asana.com/v2
`))
	if err != nil {
		t.Fatal(err)
	}
	if bad := catalogueDisagreements(l, c); len(bad) == 0 || !strings.Contains(bad[0], "mcp.asana.com/v2/mcp") {
		t.Fatalf("found %v", bad)
	}
}

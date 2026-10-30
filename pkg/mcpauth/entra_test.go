package mcpauth_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

const msaTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"

// entraDoc is OpenID discovery as an Entra v2.0 authority publishes it:
// no code_challenge_methods_supported, no revocation endpoint.
func entraDoc(host, tenant, issuer string) map[string]any {
	return map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                "https://" + host + "/" + tenant + "/oauth2/v2.0/authorize",
		"token_endpoint":                        "https://" + host + "/" + tenant + "/oauth2/v2.0/token",
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "private_key_jwt", "client_secret_basic"},
	}
}

// fakeHosts answers for every host name: each request is handled as the
// host it names, so a test can be login.microsoftonline.com. docs maps
// "<host><path>" to the discovery document served there; anything else is
// a 404, as Entra's RFC 8414 address is.
func fakeHosts(t *testing.T, docs map[string]any, extra http.HandlerFunc) *http.Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if doc, ok := docs[r.Host+r.URL.Path]; ok {
			_ = json.NewEncoder(w).Encode(doc)
			return
		}
		if extra != nil {
			extra(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	tr := srv.Client().Transport.(*http.Transport).Clone()
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	tr.TLSClientConfig.ServerName = "example.com" // the test certificate's name
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func entraDiscovery(tenant, issuer string) map[string]any {
	return map[string]any{mcpauth.EntraHost + "/" + tenant + "/v2.0/.well-known/openid-configuration": entraDoc(mcpauth.EntraHost, tenant, issuer)}
}

func TestEntraAuthoritiesAreAcceptedAsTheAuthorityAsked(t *testing.T) {
	for _, tc := range []struct{ tenant, published string }{
		{"organizations", "https://login.microsoftonline.com/{tenantid}/v2.0"},
		{"common", "https://login.microsoftonline.com/{tenantid}/v2.0"},
		{"consumers", "https://login.microsoftonline.com/" + msaTenant + "/v2.0"},
		{"72f988bf-86f1-41af-91ab-2d7cd011db47", "https://login.microsoftonline.com/72f988bf-86f1-41af-91ab-2d7cd011db47/v2.0"},
	} {
		t.Run(tc.tenant, func(t *testing.T) {
			c := fakeHosts(t, entraDiscovery(tc.tenant, tc.published), nil)
			asked := "https://login.microsoftonline.com/" + tc.tenant + "/v2.0"
			md, err := mcpauth.DiscoverIssuer(context.Background(), c, asked)
			if err != nil {
				t.Fatal(err)
			}
			if md.Issuer != asked {
				t.Fatalf("issuer = %q, want the authority asked %q", md.Issuer, asked)
			}
			if md.RevocationEndpoint != "" {
				t.Fatalf("revocation endpoint = %q, want none (Entra offers none)", md.RevocationEndpoint)
			}
			if !mcpauth.IsEntra(md.Issuer) {
				t.Fatalf("IsEntra(%q) = false", md.Issuer)
			}
		})
	}
}

func TestEntraProfileRefusals(t *testing.T) {
	const host = mcpauth.EntraHost
	other := "https://login.microsoftonline.com/11111111-2222-3333-4444-555555555555/v2.0"
	offHost := entraDoc(host, "organizations", "https://login.microsoftonline.com/{tenantid}/v2.0")
	offHost["token_endpoint"] = "https://tokens.example/organizations/oauth2/v2.0/token"
	for name, tc := range map[string]struct {
		asked string
		docs  map[string]any
		want  string
	}{
		"a multi-tenant authority naming another tenant": {
			asked: "https://login.microsoftonline.com/organizations/v2.0",
			docs:  entraDiscovery("organizations", other), want: "calls itself",
		},
		"consumers naming the placeholder": {
			asked: "https://login.microsoftonline.com/consumers/v2.0",
			docs:  entraDiscovery("consumers", "https://login.microsoftonline.com/{tenantid}/v2.0"), want: "calls itself",
		},
		"a tenant id naming another tenant": {
			asked: "https://login.microsoftonline.com/72f988bf-86f1-41af-91ab-2d7cd011db47/v2.0",
			docs:  entraDiscovery("72f988bf-86f1-41af-91ab-2d7cd011db47", other), want: "calls itself",
		},
		"the placeholder on another host": {
			asked: "https://login.microsoftonline.com/organizations/v2.0",
			docs:  entraDiscovery("organizations", "https://login.example/{tenantid}/v2.0"), want: "calls itself",
		},
		"an endpoint off Entra's host": {
			asked: "https://login.microsoftonline.com/organizations/v2.0",
			docs:  map[string]any{host + "/organizations/v2.0/.well-known/openid-configuration": offHost}, want: "off " + host,
		},
		"the v1 authority": {
			asked: "https://login.microsoftonline.com/organizations",
			docs: map[string]any{host + "/organizations/.well-known/openid-configuration": entraDoc(host, "organizations",
				"https://login.microsoftonline.com/{tenantid}/v2.0")}, want: "calls itself",
		},
		"a lookalike host with the placeholder and no S256": {
			asked: "https://login.microsoftonline.com.example/organizations/v2.0",
			docs: map[string]any{"login.microsoftonline.com.example/organizations/v2.0/.well-known/openid-configuration": entraDoc("login.microsoftonline.com.example", "organizations",
				"https://login.microsoftonline.com.example/{tenantid}/v2.0")}, want: "calls itself",
		},
		"another vendor publishing the placeholder": {
			asked: "https://auth.example/organizations/v2.0",
			docs:  map[string]any{"auth.example/organizations/v2.0/.well-known/openid-configuration": entraDoc("auth.example", "organizations", "https://auth.example/{tenantid}/v2.0")},
			want:  "calls itself",
		},
		"another vendor omitting S256": {
			asked: "https://auth.example/organizations/v2.0",
			docs:  map[string]any{"auth.example/organizations/v2.0/.well-known/openid-configuration": entraDoc("auth.example", "organizations", "https://auth.example/organizations/v2.0")},
			want:  "S256",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := mcpauth.DiscoverIssuer(context.Background(), fakeHosts(t, tc.docs, nil), tc.asked)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestIsEntraIsEntrasHostAlone(t *testing.T) {
	for issuer, want := range map[string]bool{
		"https://login.microsoftonline.com/organizations/v2.0":  true,
		"https://LOGIN.microsoftonline.com/common/v2.0/":        true,
		"https://login.microsoftonline.com:443/common/v2.0":     false,
		"https://login.microsoftonline.com.example/common/v2.0": false,
		"https://evil.login.microsoftonline.com/common/v2.0":    false,
		"http://login.microsoftonline.com/common/v2.0":          false,
		"https://login.microsoftonline.com/common":              false,
		"https://login.microsoftonline.com/common/v2.0/extra":   false,
		"https://user@login.microsoftonline.com/common/v2.0":    false,
		"https://auth.example/organizations/v2.0":               false,
	} {
		if got := mcpauth.IsEntra(issuer); got != want {
			t.Errorf("IsEntra(%q) = %v, want %v", issuer, got, want)
		}
	}
}

// A remote server whose authorization server is Entra: no resource
// parameter, which Entra refuses (AADSTS9010010), on consent or exchange.
func TestARemoteServerOnEntraSendsNoResource(t *testing.T) {
	const asked = "https://login.microsoftonline.com/organizations/v2.0"
	docs := entraDiscovery("organizations", "https://login.microsoftonline.com/{tenantid}/v2.0")
	docs["mcp.example/.well-known/oauth-protected-resource/mcp"] = map[string]any{"resource": "https://mcp.example/mcp", "authorization_servers": []string{asked}}
	c := fakeHosts(t, docs, func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "mcp.example" && r.URL.Path == "/mcp" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	})
	d, err := mcpauth.DiscoverRemote(context.Background(), c, "https://mcp.example/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if d.Resource != "" || d.Server.Issuer != asked {
		t.Fatalf("discovery = %+v, want no resource at the authority asked", d)
	}
	req, err := mcpauth.NewRequest(d.Server, mcpauth.Client{ID: "app", AuthMethod: mcpauth.AuthNone}, "http://127.0.0.1:1/callback", d.Resource, []string{"https://graph.microsoft.com/User.Read", "offline_access"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(req.URL())
	if q := u.Query(); q.Has("resource") || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("consent address = %s, want S256 and no resource", req.URL())
	}
}

func TestEntraRedirectIssuer(t *testing.T) {
	const tenant = "72f988bf-86f1-41af-91ab-2d7cd011db47"
	tenantIss := "https://login.microsoftonline.com/" + tenant + "/v2.0"
	msaIss := "https://login.microsoftonline.com/" + msaTenant + "/v2.0"
	for _, tc := range []struct {
		asked, iss string
		ok         bool
	}{
		{"https://login.microsoftonline.com/organizations/v2.0", "", true},
		{"https://login.microsoftonline.com/organizations/v2.0", tenantIss, true},
		{"https://login.microsoftonline.com/organizations/v2.0", msaIss, false},
		{"https://login.microsoftonline.com/organizations/v2.0", "https://login.microsoftonline.com/{tenantid}/v2.0", false},
		{"https://login.microsoftonline.com/organizations/v2.0", "https://login.example/" + tenant + "/v2.0", false},
		{"https://login.microsoftonline.com/common/v2.0", msaIss, true},
		{"https://login.microsoftonline.com/consumers/v2.0", msaIss, true},
		{"https://login.microsoftonline.com/consumers/v2.0", tenantIss, false},
		{"https://login.microsoftonline.com/" + tenant + "/v2.0", tenantIss, true},
		{"https://login.microsoftonline.com/" + tenant + "/v2.0", msaIss, false},
		{"https://auth.example/organizations/v2.0", "https://auth.example/" + tenant + "/v2.0", false},
	} {
		req := mcpauth.ResumeRequest(mcpauth.ServerMetadata{Issuer: tc.asked}, mcpauth.Client{ID: "app"}, "http://127.0.0.1:1/callback", "", nil, "st", "verifier")
		q := url.Values{"state": {"st"}, "code": {"c"}}
		if tc.iss != "" {
			q.Set("iss", tc.iss)
		}
		_, err := req.Callback(&url.URL{RawQuery: q.Encode()})
		if (err == nil) != tc.ok {
			t.Errorf("asked %s, iss %q: err = %v, want ok %v", tc.asked, tc.iss, err, tc.ok)
		}
	}
}

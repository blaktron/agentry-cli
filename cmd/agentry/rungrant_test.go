package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The grant goes to the registry's host and nowhere else: a
// download from another host (a CDN) and npm never see it.
func TestRunGrantReachesOnlyTheRegistryHost(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = r.Header.Get(runGrantHeader)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}
	registry := httptest.NewServer(handler("registry"))
	defer registry.Close()
	elsewhere := httptest.NewServer(handler("elsewhere"))
	defer elsewhere.Close()

	c := &http.Client{Transport: &grantTransport{next: http.DefaultTransport, grant: "agyr_grant", registry: func() string { return registry.URL }}}
	for _, u := range []string{registry.URL + "/api/v1/x/runs/rel/api/v1/resolve?ref=a/b@1.0.0", elsewhere.URL + "/archive.tgz"} {
		resp, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if seen["registry"] != "agyr_grant" {
		t.Errorf("the registry did not get the grant: %q", seen["registry"])
	}
	if seen["elsewhere"] != "" {
		t.Errorf("another host got the grant: %q", seen["elsewhere"])
	}

	// The header and the variable are the names a hosted worker uses.
	if runGrantHeader != "X-Agentry-Run-Grant" || envRunGrant != "AGENTRY_RUN_GRANT" {
		t.Errorf("the grant travels in %q, set in %q", runGrantHeader, envRunGrant)
	}

	// The grant goes to the registry's own origin: the same host over another
	// scheme, as a redirect down to plain http would reach, does not get it.
	seen["registry"] = ""
	httpsOrigin := "https://" + strings.TrimPrefix(registry.URL, "http://")
	other := &http.Client{Transport: &grantTransport{next: http.DefaultTransport, grant: "agyr_grant", registry: func() string { return httpsOrigin }}}
	resp, err := other.Get(registry.URL + "/api/v1/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if seen["registry"] != "" {
		t.Errorf("the registry's host over another scheme got the grant: %q", seen["registry"])
	}

	// The request the caller made is not changed to carry it.
	var sent *http.Request
	record := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
	})
	gt := &grantTransport{next: record, grant: "agyr_grant", registry: func() string { return "https://registry.example" }}
	req, err := http.NewRequest(http.MethodGet, "https://registry.example/api/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if sent == nil || sent.Header.Get(runGrantHeader) != "agyr_grant" {
		t.Errorf("the request sent on did not carry the grant: %+v", sent)
	}
	if req.Header.Get(runGrantHeader) != "" {
		t.Error("the caller's own request was changed")
	}
}

// Without a grant nothing is installed.
func TestNoRunGrantLeavesTheTransport(t *testing.T) {
	t.Setenv(envRunGrant, "")
	before := http.DefaultTransport
	installRunGrant()
	if http.DefaultTransport != before {
		t.Fatal("the default transport was replaced with no grant set")
	}
}

// Installed from the environment, the grant rides the CLI's own client —
// the one resolve, the archive and the attestation go through.
func TestRunGrantInstalledReachesTheCLIsClient(t *testing.T) {
	var got string
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(runGrantHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer registry.Close()
	before, beforeOrigin := http.DefaultTransport, grantOrigin
	t.Cleanup(func() { http.DefaultTransport, grantOrigin = before, beforeOrigin })
	t.Setenv(envRunGrant, "agyr_grant")
	grantOrigin = func() string { return registry.URL }
	installRunGrant()
	if wrapped, ok := http.DefaultTransport.(*grantTransport); !ok || wrapped.next != before || wrapped.grant != "agyr_grant" {
		t.Fatalf("the default transport = %#v, want the previous one wrapped with the grant", http.DefaultTransport)
	}
	resp, err := httpClient.Get(registry.URL + "/api/v1/x/runs/rel/api/v1/resolve?ref=a/b@1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "agyr_grant" {
		t.Fatalf("the CLI's client did not carry the grant: %q", got)
	}
}

// The grant goes to the scheme and host of --registry, whatever path the
// registry is served under. (The origin is worked out once per process, so
// this is the one place it is.)
func TestRunGrantOriginIsTheRegistrysSchemeAndHost(t *testing.T) {
	prev := flags.registry
	flags.registry = "https://registry.example:8443/api/prefix/"
	t.Cleanup(func() { flags.registry = prev })
	if got := grantOrigin(); got != "https://registry.example:8443" {
		t.Errorf("grantOrigin = %q", got)
	}
}

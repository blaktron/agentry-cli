package main

import (
	"net/http"
	"net/http/httptest"
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
	resp, err := httpClient.Get(registry.URL + "/api/v1/x/runs/rel/api/v1/resolve?ref=a/b@1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "agyr_grant" {
		t.Fatalf("the CLI's client did not carry the grant: %q", got)
	}
}

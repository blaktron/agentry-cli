package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The grant goes to the registry's host and nowhere else (agentry-cli#232):
// a download from another host (a CDN) and npm never see it.
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

	host, err := registryHost(registry.URL + "/api/v1/x/runs/rel")
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: &grantTransport{next: http.DefaultTransport, grant: "agyr_grant", registry: func() string { return host }}}
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/internal/version"
)

// metaServer serves GET /api/v1/meta with the given minCLIVersion ("" omits
// the field entirely, as a registry that sets no floor would).
func metaServer(t *testing.T, minCLI string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/meta", func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"version": "9.9.9", "apiVersion": "v1", "features": map[string]bool{}}
		if minCLI != "" {
			body["minCLIVersion"] = minCLI
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// gate runs one min-version check with a fresh once-guard, since the real one
// is process-wide by design.
func gate(t *testing.T, minCLI, have string) (warning string, err error) {
	t.Helper()
	old := version.Version
	version.Version = have
	t.Cleanup(func() { version.Version = old })
	t.Setenv(envSkipVersionCheck, "")

	srv := metaServer(t, minCLI)
	var buf bytes.Buffer
	err = runMinVersionCheck(context.Background(), &buf, srv.URL, "")
	return buf.String(), err
}

func TestMinVersionPolicy(t *testing.T) {
	tests := []struct {
		name        string
		minCLI      string
		have        string
		wantWarn    bool
		wantRefusal bool
	}{
		{"at the floor is silent", "1.2.0", "1.2.0", false, false},
		{"above the floor is silent", "1.2.0", "1.5.3", false, false},
		{"below within a major warns", "1.5.0", "1.2.0", true, false},
		{"a full major behind refuses", "2.0.0", "1.9.9", false, true},
		{"no floor advertised is silent", "", "0.0.1", false, false},
		{"an unparseable floor is silent", "next", "1.0.0", false, false},
		{"a dev build below the floor warns, not refuses", "0.5.0", "0.0.0-dev", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, err := gate(t, tt.minCLI, tt.have)
			if got := strings.Contains(warn, "warning:"); got != tt.wantWarn {
				t.Errorf("warned = %v, want %v (%q)", got, tt.wantWarn, warn)
			}
			if got := err != nil; got != tt.wantRefusal {
				t.Errorf("refused = %v, want %v (%v)", got, tt.wantRefusal, err)
			}
			if tt.wantRefusal && !strings.Contains(err.Error(), envSkipVersionCheck) {
				t.Errorf("refusal does not name the escape hatch: %v", err)
			}
		})
	}
}

// TestMinVersionUnreachableRegistryIsSilent: a check that cannot run is not
// evidence of anything. The command's own call reports the outage.
func TestMinVersionUnreachableRegistryIsSilent(t *testing.T) {
	old := version.Version
	version.Version = "0.1.0"
	t.Cleanup(func() { version.Version = old })

	var buf bytes.Buffer
	if err := runMinVersionCheck(context.Background(), &buf, "http://127.0.0.1:1", ""); err != nil {
		t.Fatalf("an unreachable registry refused the command: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestParseSemver(t *testing.T) {
	good := map[string][3]int{
		"1.2.3": {1, 2, 3}, "v0.1.0": {0, 1, 0},
		"2.0.0-rc.1": {2, 0, 0}, "1.0.0+build7": {1, 0, 0},
	}
	for in, want := range good {
		got, ok := parseSemver(in)
		if !ok || got != want {
			t.Errorf("parseSemver(%q) = %v,%v want %v,true", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "one.two.three", "-1.0.0"} {
		if _, ok := parseSemver(bad); ok {
			t.Errorf("parseSemver(%q) accepted a non-version", bad)
		}
	}
}

// TestValidCacheSemverRejectsPaths: the semver header names a directory, so a
// hostile registry must not be able to steer it.
func TestValidCacheSemverRejectsPaths(t *testing.T) {
	for _, bad := range []string{"../../etc", "1.0.0/../x", `1.0.0\..\x`, "v1.0.0", "..", "latest"} {
		if validCacheSemver(bad) {
			t.Errorf("validCacheSemver(%q) accepted a path-bearing name", bad)
		}
	}
	if !validCacheSemver("1.2.3") {
		t.Error("validCacheSemver rejected a plain semver")
	}
}

// `agentry validate --rules-update` (§11): the bundle is pinned by DIGEST
// (DEV-024 records why there is no signature yet) — the CLI recomputes the
// digest over what it received via the same pkg/rules code the server uses,
// and any disagreement (missing headers, wrong digest, wrong version) is a
// refusal, never a cached lie. The fixture bundle is the embedded one,
// re-served over HTTP, so digests are real.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/rules"
	"github.com/blaktron/agentry/pkg/ruleset"
)

// bundleFixture loads the embedded rule bundle and its Tar() bytes — the
// exact artifact a registry would serve.
func bundleFixture(t *testing.T) (*rules.Bundle, []byte) {
	t.Helper()
	b, err := ruleset.Load()
	if err != nil {
		t.Fatalf("embedded bundle: %v", err)
	}
	tar, err := b.Tar()
	if err != nil {
		t.Fatal(err)
	}
	return b, tar
}

// bundleServer serves the bundle endpoint with the given headers; empty
// header values are omitted.
func bundleServer(t *testing.T, tar []byte, semver, digest string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/ruleset-releases/latest/bundle", func(w http.ResponseWriter, _ *http.Request) {
		if semver != "" {
			w.Header().Set("X-Agentry-Bundle-Semver", semver)
		}
		if digest != "" {
			w.Header().Set("X-Agentry-Bundle-Digest", digest)
		}
		_, _ = w.Write(tar)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchRulesBundleHappyPathAndCache(t *testing.T) {
	isolateEnv(t)
	bundle, tar := bundleFixture(t)
	srv := bundleServer(t, tar, bundle.Version(), bundle.Digest())

	var out strings.Builder
	semver, err := fetchRulesBundle(context.Background(), &out, srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if semver != bundle.Version() {
		t.Errorf("cached semver = %q, want %q", semver, bundle.Version())
	}
	if !strings.Contains(out.String(), "pinned by digest") {
		t.Errorf("fetch narration must state the trust model: %q", out.String())
	}
	root, err := rulesCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, semver, "bundle.yaml")); err != nil {
		t.Errorf("cache layout missing bundle.yaml: %v", err)
	}

	// The freshly cached bundle is what newestCachedBundle picks up.
	got, gotVer := newestCachedBundle()
	if got == nil || gotVer != semver {
		t.Fatalf("newestCachedBundle = %v, %q", got, gotVer)
	}

	// A higher-semver cache entry wins, whatever order they arrived in.
	higher := filepath.Join(root, "999.0.0")
	if err := copyTree(filepath.Join(root, semver), higher); err != nil {
		t.Fatal(err)
	}
	if _, gotVer = newestCachedBundle(); gotVer != "999.0.0" {
		t.Errorf("newest = %q, want 999.0.0", gotVer)
	}
}

func TestFetchRulesBundleRefusals(t *testing.T) {
	isolateEnv(t)
	bundle, tar := bundleFixture(t)

	tests := []struct {
		name           string
		semver, digest string
		wantErr        string
	}{
		{"missing headers", "", "", "no semver/digest headers"},
		{"path-bearing semver", "1.0.0/../evil", bundle.Digest(), "not MAJOR.MINOR.PATCH"},
		{"digest mismatch", bundle.Version(), "sha256:" + strings.Repeat("00", 32), "digest mismatch"},
		{"version header disagrees with bundle.yaml", "9.9.9", bundle.Digest(), "version mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := bundleServer(t, tar, tt.semver, tt.digest)
			_, err := fetchRulesBundle(context.Background(), &strings.Builder{}, srv.URL)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}

	// A non-200 renders as an API problem, and nothing is cached on any
	// refusal path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "maintenance", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := fetchRulesBundle(context.Background(), &strings.Builder{}, srv.URL); err == nil ||
		!strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("non-200 fetch: %v", err)
	}
	if b, v := newestCachedBundle(); b != nil {
		t.Errorf("a refused fetch left a cached bundle %q", v)
	}
}

func TestNormalizeBundleDigest(t *testing.T) {
	tests := map[string]string{
		"":                   "",
		"abc123":             "sha256:abc123",
		"sha256:abc123":      "sha256:abc123",
		` "sha256:abc123"  `: "sha256:abc123",
		"\t\"deadbeef\"\n":   "sha256:deadbeef",
	}
	for in, want := range tests {
		if got := normalizeBundleDigest(in); got != want {
			t.Errorf("normalizeBundleDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadRulesBundlePrecedence: $AGENTRY_RULES_DIR beats ./rules beats the
// cache beats the embedded bundle — and a broken override is a hard error,
// never a silent fall-through to weaker rules (DEV-011).
func TestLoadRulesBundlePrecedence(t *testing.T) {
	isolateEnv(t)
	t.Chdir(t.TempDir()) // no ./rules dir here
	t.Setenv(envRulesDir, "")

	// Floor: the embedded bundle.
	b, source, err := loadRulesBundle()
	if err != nil || b == nil || source != "embedded" {
		t.Fatalf("embedded fallback = %v, %q, %v", b, source, err)
	}

	// A cached bundle (planted via the real fetch path) beats embedded.
	bundle, tar := bundleFixture(t)
	srv := bundleServer(t, tar, bundle.Version(), bundle.Digest())
	if _, err := fetchRulesBundle(context.Background(), &strings.Builder{}, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, source, err = loadRulesBundle(); err != nil || source != "cached via --rules-update" {
		t.Fatalf("cached source = %q, %v", source, err)
	}

	// An env override wins over everything…
	override := t.TempDir()
	if _, err := rules.UntarBundle(tar, func(name string, content []byte) error {
		return os.WriteFile(filepath.Join(override, name), content, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envRulesDir, override)
	if _, source, err = loadRulesBundle(); err != nil || source != envRulesDir {
		t.Fatalf("env source = %q, %v", source, err)
	}

	// …and a broken override refuses instead of quietly scanning with less.
	t.Setenv(envRulesDir, t.TempDir())
	if _, _, err = loadRulesBundle(); err == nil || !strings.Contains(err.Error(), envRulesDir) {
		t.Fatalf("broken env override = %v", err)
	}
}

// A signed bundle verifies against the registry's published key (pinned on
// first use); a signature over a different digest is refused and nothing is
// cached (agentry-cli#8 / agentry#25).
func TestFetchRulesBundleVerifiesSignature(t *testing.T) {
	isolateEnv(t)
	bundle, tar := bundleFixture(t)
	fr := newFakeRegistry(t)
	fr.enableSigning(t)
	fr.bundleTar, fr.bundleVer, fr.bundleDigest, fr.bundleAttest = tar, bundle.Version(), bundle.Digest(), true

	var out strings.Builder
	semver, err := fetchRulesBundle(context.Background(), &out, fr.url())
	if err != nil {
		t.Fatal(err)
	}
	if semver != bundle.Version() || !strings.Contains(out.String(), "signature verified") {
		t.Fatalf("output = %s", out.String())
	}
	if strings.Contains(out.String(), "pinned by digest only") {
		t.Fatalf("signed bundle reported as unsigned: %s", out.String())
	}

	fr.bundleBadSig = true
	root, _ := rulesCacheDir()
	_ = os.RemoveAll(root)
	out.Reset()
	if _, err := fetchRulesBundle(context.Background(), &out, fr.url()); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("bad signature accepted: err=%v out=%s", err, out.String())
	}
	if _, statErr := os.Stat(filepath.Join(root, bundle.Version())); statErr == nil {
		t.Fatal("a bundle with a bad signature was cached")
	}
}

// A registry that signs releases but not bundles is still usable: the
// bundle is pinned by digest and the output says so.
func TestFetchRulesBundleUnsignedIsReported(t *testing.T) {
	isolateEnv(t)
	bundle, tar := bundleFixture(t)
	fr := newFakeRegistry(t)
	fr.bundleTar, fr.bundleVer, fr.bundleDigest = tar, bundle.Version(), bundle.Digest()
	var out strings.Builder
	if _, err := fetchRulesBundle(context.Background(), &out, fr.url()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pinned by digest only") {
		t.Fatalf("output = %s", out.String())
	}
}

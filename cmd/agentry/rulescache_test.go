// `agentry validate --rules-update`: the bundle is pinned by DIGEST — the
// CLI recomputes the digest over what it received via the same pkg/rules
// code the server uses, and any disagreement (missing headers, wrong
// digest, wrong version) is a refusal, never a cached lie. The fixture
// bundle is the embedded one, re-served over HTTP, so digests are real.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/rules"
	"github.com/blaktron/agentry/pkg/ruleset"

	"github.com/blaktron/agentry-cli/pkg/export"
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
	if err := export.CopyTree(filepath.Join(root, semver), higher); err != nil {
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

// plantCachedBundle writes the embedded bundle into the --rules-update
// cache as the given version: its bundle.yaml declares that version and it
// sits under that directory, as a fetch of a registry at that version would.
func plantCachedBundle(t *testing.T, version string) {
	t.Helper()
	_, tar := bundleFixture(t)
	root, err := rulesCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := rules.UntarBundle(tar, func(name string, content []byte) error {
		if name == "bundle.yaml" {
			content = regexp.MustCompile(`(?m)^version: .*$`).ReplaceAll(content, []byte(`version: "`+version+`"`))
		}
		return os.WriteFile(filepath.Join(dir, name), content, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestLoadRulesBundlePrecedence: $AGENTRY_RULES_DIR beats ./rules beats a
// cached bundle newer than the embedded one beats the embedded bundle, and
// a broken override is a hard error, never a silent fall-through to weaker
// rules.
func TestLoadRulesBundlePrecedence(t *testing.T) {
	isolateEnv(t)
	t.Chdir(t.TempDir()) // no ./rules dir here
	t.Setenv(envRulesDir, "")

	// Floor: the embedded bundle.
	emb, source, err := loadRulesBundle()
	if err != nil || emb == nil || source != "embedded" {
		t.Fatalf("embedded fallback = %v, %q, %v", emb, source, err)
	}

	// A cached bundle (planted via the real fetch path) of the embedded
	// version does not displace the binary's own copy.
	bundle, tar := bundleFixture(t)
	srv := bundleServer(t, tar, bundle.Version(), bundle.Digest())
	if _, err := fetchRulesBundle(context.Background(), &strings.Builder{}, srv.URL); err != nil {
		t.Fatal(err)
	}
	if b, source, err := loadRulesBundle(); err != nil || source != "embedded" || b.Version() != emb.Version() {
		t.Fatalf("a cached bundle of the embedded version %s: source %q (%v), want embedded", emb.Version(), source, err)
	}

	// A newer cached bundle beats the embedded one.
	plantCachedBundle(t, "999.0.0")
	if b, source, err := loadRulesBundle(); err != nil || source != "cached via --rules-update" || b.Version() != "999.0.0" {
		t.Fatalf("newer cached source = %q (%v), want cached 999.0.0", source, err)
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
// cached.
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

// TestAnOlderCachedBundleYieldsToTheEmbeddedOne: a machine that ran
// `validate --rules-update` once must not keep scanning with that bundle
// after an upgrade just because the newest cached bundle was preferred
// whatever the binary embedded. A cached 0.3.0 must not hide an embedded
// 0.3.4.
func TestAnOlderCachedBundleYieldsToTheEmbeddedOne(t *testing.T) {
	isolateEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv(envRulesDir, "")
	emb, _, err := loadRulesBundle()
	if err != nil {
		t.Fatal(err)
	}
	plantCachedBundle(t, "0.0.1")
	if _, v := newestCachedBundle(); v != "0.0.1" {
		t.Fatalf("the planted cache entry is %q, want 0.0.1", v)
	}
	b, source, err := loadRulesBundle()
	if err != nil || source != "embedded" || b.Version() != emb.Version() {
		t.Fatalf("with a cached 0.0.1 under an embedded %s: %s %q (%v), want the embedded bundle", emb.Version(), b.Version(), source, err)
	}
}

// hostileEntryNames are bundle entry names that must never put a file
// outside the folder a bundle is unpacked into. Which of them a system
// refuses depends on its path rules: ".." and an absolute path leave the
// folder everywhere; on Windows a colon makes a drive-relative path
// (C:x.yaml) or names an alternate data stream (a.yaml:stream), and NUL is a
// device. Elsewhere those three are ordinary file names.
func hostileEntryNames(outside string) []struct {
	name          string
	refusedOnUnix bool
} {
	return []struct {
		name          string
		refusedOnUnix bool
	}{
		{"..", true},
		{"../escape.yaml", true},
		{filepath.Join(outside, "absolute.yaml"), true},
		{"C:x.yaml", false},
		{"NUL.yaml", false},
		{"a.yaml:stream", false},
	}
}

// onlyEntries fails the test unless dir holds exactly the entries in want.
func onlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, w := range want {
		allowed[w] = true
	}
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Errorf("%s holds %q, which no accepted write made", dir, e.Name())
		}
	}
}

func TestStagedEntryWriterKeepsEveryNameInsideTheFolder(t *testing.T) {
	outside := t.TempDir()
	// A drive-relative name resolves against the working directory.
	t.Chdir(outside)
	staging := filepath.Join(outside, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	write := stagedEntryWriter(root)

	var written []string
	for _, tt := range hostileEntryNames(outside) {
		content := []byte("entry: " + tt.name + "\n")
		err := write(tt.name, content)
		mustRefuse := tt.refusedOnUnix || runtime.GOOS == "windows" && strings.Contains(tt.name, ":")
		if mustRefuse {
			if err == nil {
				t.Errorf("write(%q) succeeded, want a refusal", tt.name)
			}
			continue
		}
		if err != nil {
			// A system may refuse more than it must (Windows refuses a
			// reserved device name); a refusal is never an escape.
			t.Logf("write(%q) refused: %v", tt.name, err)
			continue
		}
		// Accepted: it must be an ordinary file of that name in the folder.
		p := filepath.Join(staging, tt.name)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf("write(%q) accepted but left no regular file in the folder: %v", tt.name, err)
			continue
		}
		if got, err := os.ReadFile(p); err != nil || string(got) != string(content) {
			t.Errorf("write(%q) read back %q, %v", tt.name, got, err)
		}
		written = append(written, tt.name)
	}

	onlyEntries(t, outside, "staging")
	onlyEntries(t, staging, written...)
}

// hostileBundle is a bundle archive whose first entry carries name.
func hostileBundle(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range []string{name, "bundle.yaml"} {
		body := []byte("version: 1.0.0\n")
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchRulesBundleRefusesHostileEntryNames(t *testing.T) {
	isolateEnv(t)
	outside := t.TempDir()
	t.Chdir(outside)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)

	for _, tt := range hostileEntryNames(outside) {
		t.Run(tt.name, func(t *testing.T) {
			srv := bundleServer(t, hostileBundle(t, tt.name), "1.0.0", "sha256:"+strings.Repeat("00", 32))
			_, err := fetchRulesBundle(context.Background(), &strings.Builder{}, srv.URL)
			if err == nil || !strings.Contains(err.Error(), "bundle archive entry") {
				t.Errorf("err = %v, want the entry refused", err)
			}
			onlyEntries(t, outside)
			onlyEntries(t, tmp)
			if b, v := newestCachedBundle(); b != nil {
				t.Errorf("a refused bundle was cached as %q", v)
			}
		})
	}
}

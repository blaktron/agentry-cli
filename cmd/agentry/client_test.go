// client.go behavior: digest identity, RFC 9457 problem rendering, token
// scoping to the registry host, download re-rooting, resolve error
// classes, and the min-version gate wiring every API command shares.
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry/pkg/apiclient"
)

func TestSha256DigestKnownVector(t *testing.T) {
	// The empty-input SHA-256 is a published constant; getting the prefix
	// or hex casing wrong would break digest comparison everywhere.
	want := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := sha256Digest(nil); got != want {
		t.Errorf("sha256Digest(nil) = %s, want %s", got, want)
	}
}

func TestOrNone(t *testing.T) {
	if got := orNone(""); got != "none" {
		t.Errorf("orNone(\"\") = %q", got)
	}
	if got := orNone("x"); got != "x" {
		t.Errorf("orNone(\"x\") = %q", got)
	}
}

func TestAPIProblemPrefersProblemDocument(t *testing.T) {
	detail := "the range matched nothing"
	fieldErrs := []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	}{{Field: "version", Message: "not semver"}}
	p := &apiclient.Problem{Title: "not found", Detail: &detail, Errors: &fieldErrs}
	err := apiProblem("resolve x", http.StatusNotFound, p, []byte("ignored"))
	for _, want := range []string{"resolve x", "not found", "the range matched nothing", "version: not semver", "HTTP 404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("problem error %q lacks %q", err, want)
		}
	}
}

func TestAPIProblemFallsBackToBody(t *testing.T) {
	// A long non-problem body is truncated so a misbehaving proxy cannot
	// dump a page of HTML into one error line.
	long := strings.Repeat("x", 400)
	err := apiProblem("op", 502, nil, []byte(long))
	if !strings.Contains(err.Error(), "…") || len(err.Error()) > 350 {
		t.Errorf("long body not truncated: %d chars", len(err.Error()))
	}
	// An empty body falls back to the status text so the error never reads
	// as `HTTP 503: `.
	err = apiProblem("op", 503, nil, nil)
	if !strings.Contains(err.Error(), http.StatusText(http.StatusServiceUnavailable)) {
		t.Errorf("empty body error = %q", err)
	}
}

func TestManifestFromResolve(t *testing.T) {
	res := &apiclient.ResolveResult{Manifest: map[string]interface{}{
		"schemaVersion": "1", "name": "demo", "namespace": "acme", "kind": "skill",
		"version": "1.0.0", "summary": "s",
	}}
	m, err := manifestFromResolve(res)
	if err != nil || m.Name != "demo" || m.Namespace != "acme" {
		t.Fatalf("manifestFromResolve = %+v, %v", m, err)
	}

	// A manifest the typed decoder rejects (kind is not a string).
	res.Manifest = map[string]interface{}{"kind": 42}
	if _, err := manifestFromResolve(res); err == nil || !strings.Contains(err.Error(), "decode resolved manifest") {
		t.Errorf("bad manifest map: %v", err)
	}

	// A manifest json.Marshal itself cannot re-encode.
	res.Manifest = map[string]interface{}{"x": make(chan int)}
	if _, err := manifestFromResolve(res); err == nil || !strings.Contains(err.Error(), "re-encode resolved manifest") {
		t.Errorf("unencodable manifest map: %v", err)
	}
}

func TestResolveRefStates(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))

	client, err := newAPIClient(fr.url(), "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := resolveRef(context.Background(), client, "acme/demo@1.0.0", true)
	if err != nil || res.Version != "1.0.0" || res.Namespace != "acme" {
		t.Fatalf("resolve happy path: %+v, %v", res, err)
	}
	// The install-intent hint must reach the wire (it counts installs
	// without a second round-trip).
	if len(fr.installIntents) != 1 || fr.installIntents[0] != "acme/demo@1.0.0" {
		t.Errorf("install intents seen = %v", fr.installIntents)
	}

	// 404 renders the problem document, not a bare status.
	if _, err := resolveRef(context.Background(), client, "acme/missing@1.0.0", false); err == nil ||
		!strings.Contains(err.Error(), "version not found") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404 resolve error = %v", err)
	}

	// A transport failure surfaces as the wrapped error, not a nil result.
	dead, err := newAPIClient("http://127.0.0.1:1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRef(context.Background(), dead, "a/b", false); err == nil ||
		!strings.Contains(err.Error(), "resolve a/b") {
		t.Errorf("transport failure error = %v", err)
	}
}

func TestDownloadArchiveRelativeURLAndTokenScoping(t *testing.T) {
	isolateEnv(t)
	fr := newFakeRegistry(t)
	fr.addAgent("acme", "demo", "autonomous", "s").addVersion(t, "1.0.0", skillFiles("demo", ""))
	res := resolveResultOf(t, fr, "acme/demo@1.0.0")

	// Relative archiveUrl (self-host): resolves against the registry and
	// carries the bearer token because the hosts match.
	data, err := downloadArchive(context.Background(), fr.url(), "agy_tok", res)
	if err != nil {
		t.Fatal(err)
	}
	if sha256Digest(data) != res.Digest {
		t.Error("downloaded bytes do not match the advertised digest")
	}
	if len(fr.downloadAuths) != 1 || fr.downloadAuths[0] != "Bearer agy_tok" {
		t.Errorf("same-host download auth = %v, want the bearer token", fr.downloadAuths)
	}

	// Absolute archiveUrl on a DIFFERENT host (CDN): the token must NOT
	// travel — leaking a registry PAT to a CDN is the failure mode.
	var cdnAuth string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("cdn-bytes"))
	}))
	defer cdn.Close()
	res2 := *res
	res2.ArchiveUrl = cdn.URL + "/blob"
	data, err = downloadArchive(context.Background(), fr.url(), "agy_tok", &res2)
	if err != nil || string(data) != "cdn-bytes" {
		t.Fatalf("cdn download: %q, %v", data, err)
	}
	if cdnAuth != "" {
		t.Errorf("bearer token leaked to a non-registry host: %q", cdnAuth)
	}
}

// roundTripFunc lets a test answer requests without a server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDownloadArchiveTokenOnlyToRegistryOrigin(t *testing.T) {
	const registry = "https://reg.example"
	tests := []struct {
		name, archive, wantURL string
		wantToken              bool
	}{
		{"same origin", "https://reg.example/blob/1", "https://reg.example/blob/1", true},
		{"relative URL", "/blob/1", "https://reg.example/blob/1", true},
		{"http spelling of the registry's host", "http://reg.example/blob/1", "http://reg.example/blob/1", false},
		{"other port", "https://reg.example:8443/blob/1", "https://reg.example:8443/blob/1", false},
		{"other host", "https://cdn.example/blob/1", "https://cdn.example/blob/1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotURL, gotAuth string
			prev := httpClient
			httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				gotURL, gotAuth = r.URL.String(), r.Header.Get("Authorization")
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("bytes")), Request: r}, nil
			})}
			t.Cleanup(func() { httpClient = prev })
			if _, err := downloadArchive(context.Background(), registry, "agy_tok", &apiclient.ResolveResult{ArchiveUrl: tt.archive}); err != nil {
				t.Fatal(err)
			}
			want := ""
			if tt.wantToken {
				want = "Bearer agy_tok"
			}
			if gotURL != tt.wantURL || gotAuth != want {
				t.Fatalf("requested %s with Authorization %q, want %s with %q", gotURL, gotAuth, tt.wantURL, want)
			}
		})
	}
}

func TestDownloadArchiveErrorStatus(t *testing.T) {
	isolateEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone fishing", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	res := &apiclient.ResolveResult{ArchiveUrl: "/x"}
	if _, err := downloadArchive(context.Background(), srv.URL, "", res); err == nil ||
		!strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "gone fishing") {
		t.Errorf("download error = %v", err)
	}
}

// stubCmd builds a bare cobra command with buffered streams, for helpers
// that take *cobra.Command but are tested below the command tree.
func stubCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "stub", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetContext(context.Background())
	return cmd
}

// TestNewAPIClientCheckedGate: the once-per-process min-version check runs
// through newAPIClientChecked. This is deliberately the ONLY test that runs
// the gate un-skipped (every other test sets AGENTRY_SKIP_VERSION_CHECK),
// because the sync.Once makes a second un-skipped run a silent no-op.
func TestNewAPIClientCheckedGate(t *testing.T) {
	isolateEnv(t)
	t.Setenv(envSkipVersionCheck, "")
	fr := newFakeRegistry(t)
	fr.metaMinCLI = "99.0.0" // a full major beyond 0.0.0-dev: refuse

	if _, err := newAPIClientChecked(stubCmd(t), fr.url(), ""); err == nil ||
		!strings.Contains(err.Error(), "requires at least 99.0.0") {
		t.Fatalf("major-behind CLI was not refused: %v", err)
	}

	// The check is once per process: the refusal was delivered above, and
	// later invocations in the same process proceed (they already heard).
	if _, err := newAPIClientChecked(stubCmd(t), fr.url(), ""); err != nil {
		t.Fatalf("second checked client after the once-guard fired: %v", err)
	}
}

func TestMinVersionGateEnvSkip(t *testing.T) {
	t.Setenv(envSkipVersionCheck, "1")
	// No registry exists at this URL; skip must return before any I/O.
	if err := minVersionGate(context.Background(), &strings.Builder{}, "http://127.0.0.1:1", ""); err != nil {
		t.Errorf("env-skipped gate returned %v", err)
	}
}

func TestMajorOfUnparseable(t *testing.T) {
	if got := majorOf("not-a-version"); got != -1 {
		t.Errorf("majorOf garbage = %d, want -1", got)
	}
	if got := majorOf("2.1.0"); got != 2 {
		t.Errorf("majorOf(2.1.0) = %d", got)
	}
}

// TestRegistryURLPrecedence: --registry flag beats the config default beats
// the local-dev fallback.
func TestRegistryURLPrecedence(t *testing.T) {
	isolateEnv(t)

	old := flags.registry
	t.Cleanup(func() { flags.registry = old })

	flags.registry = "http://flagged.example/"
	if got := registryURL(); got != "http://flagged.example" {
		t.Errorf("flag registry = %q (trailing slash must be trimmed)", got)
	}

	flags.registry = ""
	if err := saveCLIConfig(cliConfig{DefaultRegistry: "http://configured.example/"}); err != nil {
		t.Fatal(err)
	}
	if got := registryURL(); got != "http://configured.example" {
		t.Errorf("config registry = %q", got)
	}

	isolateConfig(t) // fresh empty config dir
	if got := registryURL(); got != defaultRegistryURL {
		t.Errorf("fallback registry = %q, want %q", got, defaultRegistryURL)
	}
}

// TestNewRootCmdCommandSet: every command (plus the runner surface) is
// registered; a missing AddCommand line should fail here, not in a user's
// shell.
func TestNewRootCmdCommandSet(t *testing.T) {
	root := newRootCmd()
	want := []string{
		"init", "validate", "login", "publish", "install", "setup", "run",
		"profiles", "verify", "search", "info", "yank", "runner-tools",
	}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("root command tree lacks %q", name)
		}
	}
}

// TestVersionShowsBakedRegistry: --version names the registry the binary
// falls back to when nothing is configured, so a person (and the release
// workflow) can see which site a download talks to.
func TestVersionShowsBakedRegistry(t *testing.T) {
	saved := defaultRegistryURL
	t.Cleanup(func() { defaultRegistryURL = saved })
	defaultRegistryURL = "https://baked.example.test"

	root := newRootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "agentry version ") || lines[1] != "default registry: https://baked.example.test" {
		t.Errorf("--version = %q, want 'agentry version …' then 'default registry: https://baked.example.test'", out.String())
	}
}

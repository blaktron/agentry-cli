// Setup-flow tests through a scripted Prompter: the §P1 provider table,
// custom endpoints deriving the proxy allowlist, and the §P3 guided
// profile conversation.
package setup

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// scriptPrompter replays canned answers.
type scriptPrompter struct {
	answers []string // consumed by Ask and AskSecret in order
	confirm bool
	said    []string
}

func (s *scriptPrompter) next() string {
	if len(s.answers) == 0 {
		return ""
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a
}

func (s *scriptPrompter) Ask(_, def string) (string, error) {
	if a := s.next(); a != "" {
		return a, nil
	}
	return def, nil
}
func (s *scriptPrompter) AskSecret(string) (string, error) { return s.next(), nil }
func (s *scriptPrompter) Confirm(string, bool) (bool, error) {
	return s.confirm, nil
}
func (s *scriptPrompter) Say(t string) { s.said = append(s.said, t) }

func TestConfigureModelTableDefaults(t *testing.T) {
	pol := policy.Default()
	p := &scriptPrompter{}
	err := ConfigureModel(p, pol, ModelOpts{Provider: "openai", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if pol.Model.Provider != "openai" || pol.Model.Name != "gpt-5.2" ||
		pol.Model.APIKeyEnv != "OPENAI_API_KEY" ||
		strings.Join(pol.Model.ProxyAllow, ",") != "api.openai.com:443" {
		t.Fatalf("openai defaults not applied: %+v", pol.Model)
	}
	if err := pol.Validate(); err != nil {
		t.Fatalf("configured policy invalid: %v", err)
	}
}

func TestConfigureModelCustomDerivesProxy(t *testing.T) {
	pol := policy.Default()
	p := &scriptPrompter{}
	err := ConfigureModel(p, pol, ModelOpts{
		Provider: "custom", Model: "llama-local", APIKeyEnv: "LOCAL_KEY",
		BaseURL: "https://llm.internal:8443/v1", Yes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pol.Model.ProxyAllow, ",") != "llm.internal:8443" {
		t.Fatalf("proxy allow = %v", pol.Model.ProxyAllow)
	}
	if pol.Model.BaseURL != "https://llm.internal:8443/v1" {
		t.Fatalf("base url lost: %q", pol.Model.BaseURL)
	}
}

func TestConfigureModelCustomNeedsBaseURLInYesMode(t *testing.T) {
	pol := policy.Default()
	err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "custom", Model: "m", APIKeyEnv: "K", Yes: true})
	if err == nil || !strings.Contains(err.Error(), "base-url") {
		t.Fatalf("custom without base-url must error, got %v", err)
	}
}

// TestConfigureModelYesKeepsTheSameProvidersBaseURL (agentry-cli#188): the
// same-provider rule — keep what the operator customized — covers the base
// URL as it covers the model and the variable, so an unattended re-run on
// a custom policy needs no --base-url and derives the allowlist from the
// URL it kept. A provider switch to custom with no URL is still refused.
func TestConfigureModelYesKeepsTheSameProvidersBaseURL(t *testing.T) {
	pol := policy.Default()
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{
		Provider: "custom", Model: "llama-local", BaseURL: "https://llm.internal:8443/v1", Yes: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Three re-run shapes on the same policy, in order; the model is kept
	// until the third names another.
	for i, opts := range []ModelOpts{{Yes: true}, {Provider: "custom", Yes: true}, {Provider: "custom", Model: "llama-tune", Yes: true}} {
		if err := ConfigureModel(&scriptPrompter{}, pol, opts); err != nil {
			t.Fatalf("re-run %+v on a custom policy: %v", opts, err)
		}
		if pol.Model.Provider != "custom" || pol.Model.BaseURL != "https://llm.internal:8443/v1" || strings.Join(pol.Model.ProxyAllow, ",") != "llm.internal:8443" {
			t.Fatalf("re-run %+v lost the URL: %+v", opts, pol.Model)
		}
		if want := map[bool]string{false: "llama-local", true: "llama-tune"}[i == 2]; pol.Model.Name != want {
			t.Fatalf("re-run %d model = %q, want %q", i, pol.Model.Name, want)
		}
	}
	// On a terminal the question is still asked, with the kept URL as its
	// default: an empty answer keeps it, a typed one moves the endpoint.
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "custom"}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.BaseURL != "https://llm.internal:8443/v1" {
		t.Fatalf("interactive re-run lost the URL: %+v", pol.Model)
	}
	moved := &scriptPrompter{answers: []string{"", "", "https://llm.moved:8443/v1"}} // model, variable, URL
	if err := ConfigureModel(moved, pol, ModelOpts{Provider: "custom"}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.BaseURL != "https://llm.moved:8443/v1" || strings.Join(pol.Model.ProxyAllow, ",") != "llm.moved:8443" {
		t.Fatalf("interactive re-run did not take the new URL: %+v", pol.Model)
	}
	// A new URL on the same provider replaces the kept one, allowlist included.
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "custom", BaseURL: "https://llm.other:9443/v1", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.BaseURL != "https://llm.other:9443/v1" || strings.Join(pol.Model.ProxyAllow, ",") != "llm.other:9443" {
		t.Fatalf("new URL not taken: %+v", pol.Model)
	}
	// Leaving custom for a table provider drops the URL with the provider;
	// coming back to custom is a switch again, and needs the URL.
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "openai", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.BaseURL != "" {
		t.Fatalf("a table provider kept a custom URL: %+v", pol.Model)
	}
	err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "custom", Model: "m", Yes: true})
	if err == nil || !strings.Contains(err.Error(), "base-url") {
		t.Fatalf("switch to custom without a URL must still refuse, got %v", err)
	}
}

func TestConfigureModelInteractive(t *testing.T) {
	pol := policy.Default()
	p := &scriptPrompter{answers: []string{"google", "", ""}} // provider; model+keyEnv keep defaults
	if err := ConfigureModel(p, pol, ModelOpts{}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.Provider != "google" || pol.Model.Name != "gemini-3-pro" || pol.Model.APIKeyEnv != "GEMINI_API_KEY" {
		t.Fatalf("interactive google config: %+v", pol.Model)
	}
}

func TestGuidedProfile(t *testing.T) {
	p := &scriptPrompter{answers: []string{"first-answer", ""}, confirm: true}
	prof, err := GuidedProfile(p, GuidedProfileOpts{
		Ref: "demo/x@1.0.0", Digest: "sha256:abcdef",
		Secrets:     []string{"TOKEN_B", "TOKEN_A"}, // prompted sorted: A first
		DefaultKeep: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prof.Secrets["TOKEN_A"] != "first-answer" {
		t.Fatalf("TOKEN_A (first sorted prompt) got %q", prof.Secrets["TOKEN_A"])
	}
	if _, present := prof.Secrets["TOKEN_B"]; present {
		t.Fatal("empty answer must be recorded as absent, not empty")
	}
	if !prof.Keep {
		t.Fatal("confirm=true must set keep")
	}
	if prof.Ref != "demo/x@1.0.0" || prof.ProfileVersion != "1" {
		t.Fatalf("profile identity: %+v", prof)
	}
}

func TestGuidedProfileKeepDecidedSkipsQuestion(t *testing.T) {
	p := &scriptPrompter{confirm: false} // would answer "no" if asked
	prof, err := GuidedProfile(p, GuidedProfileOpts{
		Ref: "demo/x@1.0.0", Digest: "sha256:abcdef",
		DefaultKeep: true, KeepDecided: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prof.Keep {
		t.Fatal("KeepDecided must bypass the confirm and use DefaultKeep")
	}
}

// failingPrompter errors on the Nth interaction (1-based), counting Ask,
// AskSecret, and Confirm together — how a closed stdin surfaces mid-flow.
type failingPrompter struct {
	failOn int
	n      int
	err    error
}

func (f *failingPrompter) step() error {
	f.n++
	if f.n >= f.failOn {
		return f.err
	}
	return nil
}

func (f *failingPrompter) Ask(_, def string) (string, error)      { return def, f.step() }
func (f *failingPrompter) AskSecret(string) (string, error)       { return "v", f.step() }
func (f *failingPrompter) Confirm(_ string, d bool) (bool, error) { return d, f.step() }
func (f *failingPrompter) Say(string)                             {}

func TestConfigureModelPrompterFailures(t *testing.T) {
	// Every interactive question must propagate a read failure instead of
	// configuring a half-answered model block.
	boom := errStub("stdin closed")
	for failOn := 1; failOn <= 3; failOn++ {
		pol := policy.Default()
		p := &failingPrompter{failOn: failOn, err: boom}
		if err := ConfigureModel(p, pol, ModelOpts{}); err == nil {
			t.Errorf("failure on interaction %d was swallowed", failOn)
		}
	}
	// Custom-endpoint question (base URL) is a fourth ask.
	pol := policy.Default()
	p := &failingPrompter{failOn: 4, err: boom}
	if err := ConfigureModel(p, pol, ModelOpts{Provider: "custom"}); err == nil {
		t.Error("base-url ask failure was swallowed")
	}
}

// errStub is a trivial error type so the tests need no fmt.
type errStub string

func (e errStub) Error() string { return string(e) }

func TestConfigureModelUnknownProviderTreatedAsCustom(t *testing.T) {
	pol := policy.Default()
	p := &scriptPrompter{}
	err := ConfigureModel(p, pol, ModelOpts{
		Provider: "acme-llm", Model: "m1", APIKeyEnv: "ACME_KEY",
		BaseURL: "https://llm.acme.test:9443/v1", Yes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pol.Model.Provider != "acme-llm" || strings.Join(pol.Model.ProxyAllow, ",") != "llm.acme.test:9443" {
		t.Fatalf("unlisted provider config: %+v", pol.Model)
	}
	found := false
	for _, s := range p.said {
		if strings.Contains(s, "not in the built-in table") {
			found = true
		}
	}
	if !found {
		t.Errorf("operator was not told the provider is unlisted; said = %q", p.said)
	}
}

func TestConfigureModelYesModeKeepsExistingProvider(t *testing.T) {
	pol := policy.Default() // provider anthropic, model claude-sonnet-4-5
	pol.Model.Name = "claude-custom-tune"
	err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if pol.Model.Provider != "anthropic" || pol.Model.Name != "claude-custom-tune" {
		t.Fatalf("--yes with no provider must keep the existing choice: %+v", pol.Model)
	}
}

func TestConfigureModelNoModelChosen(t *testing.T) {
	pol := policy.Default()
	pol.Model.Provider, pol.Model.Name = "", "" // fresh policy, unlisted provider
	err := ConfigureModel(&scriptPrompter{answers: []string{"weird-llm"}}, pol, ModelOpts{})
	if err == nil || !strings.Contains(err.Error(), "no model chosen") {
		t.Fatalf("err = %v", err)
	}
}

func TestGuidedProfilePrompterFailures(t *testing.T) {
	boom := errStub("stdin closed")
	if _, err := GuidedProfile(&failingPrompter{failOn: 1, err: boom}, GuidedProfileOpts{
		Ref: "demo/x@1.0.0", Digest: "sha256:abc", Secrets: []string{"TOKEN"},
	}); err == nil {
		t.Error("AskSecret failure was swallowed")
	}
	if _, err := GuidedProfile(&failingPrompter{failOn: 1, err: boom}, GuidedProfileOpts{
		Ref: "demo/x@1.0.0", Digest: "sha256:abc",
	}); err == nil {
		t.Error("Confirm failure was swallowed")
	}
}

func TestGuidedProfileShowsContractAndDiscardChoice(t *testing.T) {
	p := &scriptPrompter{confirm: false}
	prof, err := GuidedProfile(p, GuidedProfileOpts{
		Ref: "demo/x@1.0.0", Digest: "sha256:abcdef1234567890",
		CapabilityLines: []string{"network: api.example.com", "secrets: TOKEN"},
		DefaultKeep:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prof.Keep {
		t.Fatal("confirm=false must record a discard choice")
	}
	said := strings.Join(p.said, "\n")
	for _, want := range []string{"Declared contract:", "network: api.example.com", "abcdef123456", "removed when the run completes"} {
		if !strings.Contains(said, want) {
			t.Errorf("guided setup output missing %q in:\n%s", want, said)
		}
	}
}

func TestProvidersAndDefaultsFor(t *testing.T) {
	got := Providers()
	if strings.Join(got, ",") != "anthropic,openai,google,custom" {
		t.Errorf("Providers() = %v", got)
	}
	if d := DefaultsFor("openai"); d == nil || d.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("DefaultsFor(openai) = %+v", d)
	}
	if DefaultsFor("custom") != nil || DefaultsFor("nope") != nil {
		t.Error("custom/unknown providers must have no table row")
	}
}

func TestShortDigest(t *testing.T) {
	tests := []struct{ in, want string }{
		{"sha256:abcdef1234567890ffff", "abcdef123456"},
		{"abcdef1234567890ffff", "abcdef123456"},
		{"sha256:short", "short"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := shortDigest(tc.in); got != tc.want {
			t.Errorf("shortDigest(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDefaultOr(t *testing.T) {
	if got := defaultOr("set", "def"); got != "set" {
		t.Errorf("defaultOr kept %q", got)
	}
	if got := defaultOr("", "def"); got != "def" {
		t.Errorf("defaultOr fell back to %q", got)
	}
}

func TestHostPortOf(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"https://api.example.com/v1", "api.example.com:443", false},
		{"http://llm.lan:8080", "llm.lan:8080", false},
		{"http://llm.lan", "llm.lan:80", false},
		{"not a url", "", true},
		{"", "", true},
	}
	for _, tc := range tests {
		got, err := hostPortOf(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("hostPortOf(%q) = (%q, %v)", tc.in, got, err)
		}
	}
}

// TestConfigureModelStoresTheKeyInTheKeychain (agentry-cli#148): on a
// terminal the flow offers to store the key; the secret goes to the store
// seam and the policy records only the source.
func TestConfigureModelStoresTheKeyInTheKeychain(t *testing.T) {
	var stored []string
	p := &scriptPrompter{answers: []string{"anthropic", "", "", "sk-live-secret"}, confirm: true}
	pol := policy.Default()
	err := ConfigureModel(p, pol, ModelOpts{StoreKey: func(prov, key string) error {
		stored = append(stored, prov+"="+key)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0] != "anthropic=sk-live-secret" {
		t.Fatalf("stored = %v", stored)
	}
	if pol.Model.APIKeySource != "keyring" || pol.Model.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("model = %+v", pol.Model)
	}
	if strings.Contains(strings.Join(p.said, "\n"), "sk-live") {
		t.Fatalf("the secret reached the transcript: %q", p.said)
	}
	dir := t.TempDir()
	if err := policy.Save(dir+"/p.yaml", pol); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(dir + "/p.yaml")
	if strings.Contains(string(raw), "sk-live") || !strings.Contains(string(raw), "apiKeySource: keyring") {
		t.Fatalf("policy file:\n%s", raw)
	}
}

// TestConfigureModelDeclinedStoreKeepsEnv: declining the offer, or an
// empty key, leaves the source unset and stores nothing.
func TestConfigureModelDeclinedStoreKeepsEnv(t *testing.T) {
	calls := 0
	store := func(string, string) error { calls++; return nil }
	pol := policy.Default()
	if err := ConfigureModel(&scriptPrompter{answers: []string{"anthropic"}, confirm: false}, pol, ModelOpts{StoreKey: store}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeySource != "" || calls != 0 {
		t.Fatalf("declined: source=%q calls=%d", pol.Model.APIKeySource, calls)
	}
	pol = policy.Default()
	p := &scriptPrompter{answers: []string{"anthropic", "", "", "   "}, confirm: true}
	if err := ConfigureModel(p, pol, ModelOpts{StoreKey: store}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeySource != "" || calls != 0 || !strings.Contains(strings.Join(p.said, "\n"), "nothing stored") {
		t.Fatalf("empty key: source=%q calls=%d said=%q", pol.Model.APIKeySource, calls, p.said)
	}
}

// TestConfigureModelYesKeepsAndSetsTheSource: a form-driven caller names
// the source; re-running for the same provider keeps a keyring source;
// an unknown source is refused.
func TestConfigureModelYesKeepsAndSetsTheSource(t *testing.T) {
	pol := policy.Default()
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "anthropic", APIKeySource: "keyring", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeySource != "keyring" {
		t.Fatalf("set: %+v", pol.Model)
	}
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "anthropic", Model: "claude-opus-5", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeySource != "keyring" || pol.Model.Name != "claude-opus-5" {
		t.Fatalf("same provider must keep the source: %+v", pol.Model)
	}
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "openai", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeySource != "" {
		t.Fatalf("a provider switch drops the other provider's source: %+v", pol.Model)
	}
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "openai", APIKeySource: "vault", Yes: true}); err == nil || !strings.Contains(err.Error(), "must be env or keyring") {
		t.Fatalf("vault: %v", err)
	}
}

// TestConfigureModelStoreFailureDegradesToEnv (agentry-cli#148): a
// keychain that cannot be reached leaves the policy on the variable path,
// written, with the reason said and no secret in the transcript.
func TestConfigureModelStoreFailureDegradesToEnv(t *testing.T) {
	p := &scriptPrompter{answers: []string{"anthropic", "", "", "sk-live-secret"}, confirm: true}
	pol := policy.Default()
	err := ConfigureModel(p, pol, ModelOpts{StoreKey: func(string, string) error { return errStub("no Secret Service") }})
	if err != nil {
		t.Fatalf("a store failure must not fail setup: %v", err)
	}
	said := strings.Join(p.said, "\n")
	if pol.Model.APIKeySource != "" || !strings.Contains(said, "not stored") || !strings.Contains(said, "no Secret Service") || strings.Contains(said, "sk-live") {
		t.Fatalf("source=%q said=%q", pol.Model.APIKeySource, said)
	}
}

// TestConfigureModelExplicitSourceSkipsTheOffer: a caller that named the
// source is not asked; the headless switch silences the offer too.
func TestConfigureModelExplicitSourceSkipsTheOffer(t *testing.T) {
	calls := 0
	store := func(string, string) error { calls++; return nil }
	p := &confirmCounter{}
	if err := ConfigureModel(p, policy.Default(), ModelOpts{Provider: "anthropic", APIKeySource: "env", StoreKey: store}); err != nil {
		t.Fatal(err)
	}
	if p.confirms != 0 || calls != 0 {
		t.Fatalf("explicit env: confirms=%d calls=%d", p.confirms, calls)
	}
	t.Setenv("AGENTRY_NO_KEYRING", "1")
	p = &confirmCounter{}
	if err := ConfigureModel(p, policy.Default(), ModelOpts{Provider: "anthropic", StoreKey: store}); err != nil {
		t.Fatal(err)
	}
	if p.confirms != 0 {
		t.Fatalf("disabled keychain: confirms=%d", p.confirms)
	}
}

// confirmCounter answers every question with its default and counts the
// Confirms.
type confirmCounter struct{ confirms int }

func (c *confirmCounter) Ask(_, def string) (string, error)      { return def, nil }
func (c *confirmCounter) AskSecret(string) (string, error)       { return "", nil }
func (c *confirmCounter) Confirm(_ string, d bool) (bool, error) { c.confirms++; return d, nil }
func (c *confirmCounter) Say(string)                             {}

// TestConfigureModelOfferFailuresPropagate: a read failure on the offer's
// Confirm or its AskSecret is an error like the other questions'.
func TestConfigureModelOfferFailuresPropagate(t *testing.T) {
	boom := errStub("stdin closed")
	for failOn := 4; failOn <= 5; failOn++ {
		pol := policy.Default()
		p := &failingConfirmingPrompter{failingPrompter: failingPrompter{failOn: failOn, err: boom}}
		if err := ConfigureModel(p, pol, ModelOpts{StoreKey: func(string, string) error { return nil }}); err == nil {
			t.Errorf("failure on interaction %d was swallowed", failOn)
		}
	}
}

// failingConfirmingPrompter says yes to Confirms so the AskSecret is reached.
type failingConfirmingPrompter struct{ failingPrompter }

func (f *failingConfirmingPrompter) Confirm(string, bool) (bool, error) { return true, f.step() }

// The credential's kind (agentry-cli#171) is read from the key at the
// one moment the flow holds it: a subscription token stored for
// anthropic records apiKeyKind subscription and says what it drives; an
// API key records nothing (api is the default, unwritten); a
// variable-only setup takes the kind as an option; a switch of provider
// drops a kind that was the old provider's; a bad kind is refused.
func TestConfigureModelRecordsTheCredentialKind(t *testing.T) {
	store := func(string, string) error { return nil }
	p := &scriptPrompter{answers: []string{"anthropic", "", "", "sk-ant-oat01-secret"}, confirm: true}
	pol := policy.Default()
	if err := ConfigureModel(p, pol, ModelOpts{StoreKey: store}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeyKind != "subscription" {
		t.Fatalf("kind = %q", pol.Model.APIKeyKind)
	}
	said := strings.Join(p.said, "\n")
	if !strings.Contains(said, "Claude Code only") || strings.Contains(said, "secret") {
		t.Fatalf("said = %q", p.said)
	}
	// The shared sentence is said as it stands, never after a prefix that
	// breaks its grammar (agentry-cli#183).
	if !slices.Contains(p.said, policy.SubscriptionTokenSentence) || strings.Contains(said, "the key is an Anthropic") {
		t.Fatalf("the sentence is not said on its own line: %q", p.said)
	}

	p = &scriptPrompter{answers: []string{"anthropic", "", "", "sk-ant-api03-secret"}, confirm: true}
	pol = policy.Default()
	if err := ConfigureModel(p, pol, ModelOpts{StoreKey: store}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeyKind != "" || strings.Contains(strings.Join(p.said, "\n"), "Claude Code only") {
		t.Fatalf("an API key: kind = %q, said = %q", pol.Model.APIKeyKind, p.said)
	}

	pol = policy.Default()
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "anthropic", APIKeyKind: "subscription", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if pol.Model.APIKeyKind != "subscription" {
		t.Fatalf("option: kind = %q", pol.Model.APIKeyKind)
	}
	// Staying on the provider keeps the kind; switching drops it.
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "anthropic", Model: "claude-opus-5", Yes: true}); err != nil || pol.Model.APIKeyKind != "subscription" {
		t.Fatalf("same provider: kind = %q %v", pol.Model.APIKeyKind, err)
	}
	if err := ConfigureModel(&scriptPrompter{}, pol, ModelOpts{Provider: "openai", Yes: true}); err != nil || pol.Model.APIKeyKind != "" {
		t.Fatalf("switched provider: kind = %q %v", pol.Model.APIKeyKind, err)
	}
	if err := ConfigureModel(&scriptPrompter{}, policy.Default(), ModelOpts{Provider: "anthropic", APIKeyKind: "oauth", Yes: true}); err == nil || !strings.Contains(err.Error(), "api key kind") {
		t.Fatalf("bad kind: %v", err)
	}
}

// TestGuidedProfileSaysEachKeysHelp: the release guide's connect help
// (DEV-141, agentry-cli#257) is printed before the question for that key,
// and a key without help gets none.
func TestGuidedProfileSaysEachKeysHelp(t *testing.T) {
	p := &scriptPrompter{confirm: true}
	_, err := GuidedProfile(p, GuidedProfileOpts{
		Ref: "demo/x@1.0.0", Digest: "sha256:abcdef",
		Secrets: []string{"GITHUB_TOKEN", "OTHER"},
		Help:    map[string]string{"GITHUB_TOKEN": "A fine-grained token with read access to pull requests"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var help []string
	for _, s := range p.said {
		if strings.Contains(s, ": ") && (strings.HasPrefix(s, "GITHUB_TOKEN") || strings.HasPrefix(s, "OTHER")) {
			help = append(help, s)
		}
	}
	if len(help) != 1 || help[0] != "GITHUB_TOKEN: A fine-grained token with read access to pull requests" {
		t.Fatalf("help said = %v", help)
	}
}

// A custom endpoint over plain http off this machine is configured, with
// the warning said (agentry-cli#377); https and loopback say nothing.
func TestConfigureModelCustomPlainHTTPIsAllowedWithAWarning(t *testing.T) {
	for _, tc := range []struct {
		url  string
		warn bool
	}{
		{"http://gpu-box.lan:8000/v1", true},
		{"https://llm.internal:8443/v1", false},
		{"http://127.0.0.1:11434/v1", false},
	} {
		pol := policy.Default()
		p := &scriptPrompter{}
		if err := ConfigureModel(p, pol, ModelOpts{Provider: "custom", Model: "m", APIKeyEnv: "K", BaseURL: tc.url, Yes: true}); err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if pol.Model.BaseURL != tc.url {
			t.Fatalf("%s: base url = %q", tc.url, pol.Model.BaseURL)
		}
		warned := strings.Contains(strings.Join(p.said, "\n"), "warning: ")
		if warned != tc.warn {
			t.Errorf("%s: warned=%v, want %v (said %q)", tc.url, warned, tc.warn, p.said)
		}
		if err := pol.Validate(); err != nil {
			t.Fatalf("%s: configured policy invalid: %v", tc.url, err)
		}
	}
}

// An IPv6 endpoint's allowlist entry is bracketed, the form the proxy
// parses (agentry-cli#377 review).
func TestHostPortOfBracketsIPv6(t *testing.T) {
	for in, want := range map[string]string{
		"http://[fe80::1]:8000/v1": "[fe80::1]:8000",
		"https://llm.lan/v1":       "llm.lan:443",
		"http://llm.lan/v1":        "llm.lan:80",
	} {
		if got, err := hostPortOf(in); err != nil || got != want {
			t.Errorf("hostPortOf(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestGuidedProfileQuotesAMultiLineHelp (agentry-cli#419, DEV-167): a
// key's help is markdown that may span lines, so it is said beneath the
// key's name with every line behind termtext's gutter. A help line that
// reads like the CLI's own (a prompt for another key, a contract line)
// cannot start where the CLI's lines start, and what reaches the terminal
// is cleaned: the core refuses control characters, and this is the second
// defence.
func TestGuidedProfileQuotesAMultiLineHelp(t *testing.T) {
	p := &scriptPrompter{confirm: true}
	_, err := GuidedProfile(p, GuidedProfileOpts{
		Ref: "demo/x@1.0.0", Digest: "sha256:abcdef",
		CapabilityLines: []string{"egress: api.github.com"},
		Secrets:         []string{"GITHUB_TOKEN"},
		Help: map[string]string{"GITHUB_TOKEN": "1. Open **Settings → Tokens**.\n" +
			"Value for OTHER_TOKEN (empty = resolve from the environment at run time)\n\n" +
			"Declared contract:\n  egress: *\x1b]0;x\x07\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var help string
	for _, s := range p.said {
		if strings.HasPrefix(s, "GITHUB_TOKEN:") {
			help = s
		}
	}
	want := "GITHUB_TOKEN:\n" +
		"  | 1. Open **Settings → Tokens**.\n" +
		"  | Value for OTHER_TOKEN (empty = resolve from the environment at run time)\n" +
		"  |\n" +
		"  | Declared contract:\n" +
		"  |   egress: *?]0;x?"
	if help != want {
		t.Fatalf("help said =\n%s\nwant\n%s", help, want)
	}
	// The CLI's own contract line, for contrast, is not behind the gutter.
	var contract bool
	for _, s := range p.said {
		contract = contract || s == "  egress: api.github.com"
	}
	if !contract {
		t.Fatalf("the contract line moved: %q", p.said)
	}
}

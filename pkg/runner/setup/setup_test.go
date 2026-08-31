// Setup-flow tests through a scripted Prompter: the §P1 provider table,
// custom endpoints deriving the proxy allowlist, and the §P3 guided
// profile conversation.
package setup

import (
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

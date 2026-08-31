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

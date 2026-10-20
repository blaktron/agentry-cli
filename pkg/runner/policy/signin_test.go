package policy

import (
	"slices"
	"strings"
	"testing"
)

const (
	testDigest    = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testIntegrity = "sha512-OX/k/rpcthMqnFKOWcpNcbiLsMKtAjLOpQNJHlYiDCFUznWCIwdRTyl/p9SdHVrsrzZuPMCNqZjnpakIni6upw=="
)

// Each model sign-in kind is admitted beside the one provider it spends,
// in the model block and a harness block alike, and refused beside any
// other provider by name.
func TestAModelSignInKindIsHeldToItsProvider(t *testing.T) {
	for kind, provider := range map[string]string{
		KeyKindChatGPTPlan:   ProviderOpenAI,
		KeyKindClaudeSignIn:  ProviderAnthropic,
		KeyKindCopilotSignIn: ProviderCopilot,
	} {
		p := Default()
		p.Model.Provider, p.Model.APIKeyKind = provider, kind
		if err := p.Validate(); err != nil {
			t.Errorf("%s beside %s: %v", kind, provider, err)
		}
		p.Model.Provider = "google"
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "needs model.provider "+provider) {
			t.Errorf("%s beside google: %v", kind, err)
		}
		p = Default()
		p.Model.Provider = provider
		p.Harness = map[string]HarnessPolicy{"codex": {APIKeyKind: kind}}
		if err := p.Validate(); err != nil {
			t.Errorf("harness %s beside %s: %v", kind, provider, err)
		}
	}
	p := Default()
	p.Model.APIKeyKind = "oauth"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "chatgpt-plan") {
		t.Fatalf("an unknown kind: %v", err)
	}
}

// A model sign-in needs no key variable: it is not a key.
func TestAModelSignInNeedsNoKeyVariable(t *testing.T) {
	p := Default()
	p.Model.Provider, p.Model.APIKeyKind, p.Model.APIKeyEnv = ProviderOpenAI, KeyKindChatGPTPlan, ""
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Model.APIKeyKind = KeyKindAPI
	if err := p.Validate(); err == nil {
		t.Fatal("an API key with no variable must be refused")
	}
}

// A cost budget is refused beside a model sign-in, in either block.
func TestACostBudgetIsRefusedBesideAModelSignIn(t *testing.T) {
	p := Default()
	p.Model.Provider, p.Model.APIKeyKind = ProviderOpenAI, KeyKindChatGPTPlan
	p.Budgets.MaxCostUSD = 1
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "tokens, not dollars") {
		t.Fatalf("model block: %v", err)
	}
	p = Default()
	p.Budgets.MaxCostUSD = 1
	p.Harness = map[string]HarnessPolicy{"claude": {APIKeyKind: KeyKindClaudeSignIn}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "maxCostUsd") {
		t.Fatalf("harness block: %v", err)
	}
	p.Harness = nil
	if err := p.Validate(); err != nil {
		t.Fatalf("an API key keeps its cost budget: %v", err)
	}
}

// A harness image is pinned by digest or by the artifact it is built
// from, never by a tag; docker-agent's entry is resolved by Load, so one
// left in the map is refused.
func TestHarnessImagesArePinned(t *testing.T) {
	for _, c := range []struct {
		name string
		img  HarnessImage
		ok   bool
		by   string
	}{
		{"digest", HarnessImage{Ref: "ghcr.io/blaktron/agentry-codex:0.162.0@" + testDigest}, true, PinDigest},
		{"artifact", HarnessImage{Ref: "agentry-harness-claude:2.1.300", Artifact: testDigest}, true, PinArtifact},
		{"tag", HarnessImage{Ref: "ghcr.io/blaktron/agentry-codex:0.162.0"}, false, ""},
		{"artifact on a pulled name", HarnessImage{Ref: "ghcr.io/x/claude:1", Artifact: testDigest}, false, ""},
		{"short artifact", HarnessImage{Ref: "agentry-harness-claude:1", Artifact: "sha256:abc"}, false, ""},
		{"npm integrity", HarnessImage{Ref: "agentry-claude-code:2.1.296", Artifact: testIntegrity}, true, PinArtifact},
		{"short npm integrity", HarnessImage{Ref: "agentry-claude-code:2.1.296", Artifact: "sha512-OX/k=="}, false, ""},
		{"sha1 integrity", HarnessImage{Ref: "agentry-claude-code:2.1.296", Artifact: "sha1-wHwya2/f9vHCDIoatjqa+n85W6U="}, false, ""},
	} {
		p := Default()
		p.Images.HarnessImages = map[string]HarnessImage{"codex": c.img}
		err := p.Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if c.ok {
			got, ok := p.Images.HarnessImageFor("codex")
			by, val := got.Pin()
			if !ok || by != c.by || val != c.img.Artifact && val != testDigest {
				t.Errorf("%s: pin %s %s (%v)", c.name, by, val, ok)
			}
		}
	}
	p := Default()
	p.Images.HarnessImages = map[string]HarnessImage{DockerAgentAdapter: {Ref: "x@" + testDigest}}
	if err := p.Validate(); err == nil {
		t.Fatal("docker-agent in harnessImages must be refused")
	}
}

// The default harness image is pinned by digest; a policy file naming a
// tag still loads, and its pin says tag; a host adapter has no image.
func TestTheDefaultHarnessImageIsPinnedByDigest(t *testing.T) {
	h, ok := Default().Images.HarnessImageFor(DockerAgentAdapter)
	if by, val := h.Pin(); !ok || by != PinDigest || !strings.HasPrefix(val, "sha256:") || len(val) != 71 {
		t.Fatalf("default: %s %s (%v)", by, val, ok)
	}
	p := Default()
	p.Images.HarnessDockerAgent = "ghcr.io/blaktron/agentry-docker-agent:1.128.0"
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if by, _ := (HarnessImage{Ref: p.Images.HarnessDockerAgent}).Pin(); by != PinTag {
		t.Fatalf("a tag pins by %s", by)
	}
	if _, ok := p.Images.HarnessImageFor("claude"); ok {
		t.Fatal("the host claude adapter has no default image")
	}
}

// Claude Code's image is built on the machine, never pulled: its
// default is a local name pinned by the npm integrity of the release it is
// built from, and a Claude run reaches Anthropic's API and the host Claude
// Code refreshes its own sign-in at, nothing else.
func TestClaudeCodesImageIsPinnedByItsNpmIntegrity(t *testing.T) {
	h, ok := Default().Images.HarnessImageFor("claude-image")
	if by, val := h.Pin(); !ok || by != PinArtifact || !strings.HasPrefix(val, "sha512-") || namesRegistry(h.Ref) {
		t.Fatalf("claude-image: %s %s %q (%v)", by, val, h.Ref, ok)
	}
	if err := h.validate("claude-image"); err != nil {
		t.Fatal(err)
	}
	if got := SignInProxyAllow(KeyKindClaudeSignIn); !slices.Equal(got, []string{"api.anthropic.com:443", "platform.claude.com:443"}) {
		t.Fatalf("claude-signin endpoints %v", got)
	}
}

// harness.<name>.signInTokenRoute is auth-command or restart, nothing else;
// unset is the auth command.
func TestASignInTokenRouteIsOneOfTwo(t *testing.T) {
	for route, ok := range map[string]bool{"": true, TokenRouteAuthCommand: true, TokenRouteRestart: true, "env": false, "Restart": false} {
		p := Default()
		p.Harness = map[string]HarnessPolicy{"codex-image": {SignInTokenRoute: route}}
		err := p.Validate()
		if (err == nil) != ok {
			t.Errorf("route %q: %v", route, err)
		}
	}
	if SignInTokenRoute(HarnessPolicy{}) != TokenRouteAuthCommand {
		t.Error("an unset route is not the auth command")
	}
	allow := SignInProxyAllow(KeyKindChatGPTPlan)
	allow[0] = "elsewhere:443"
	if SignInProxyAllow(KeyKindChatGPTPlan)[0] != "api.openai.com:443" {
		t.Error("a caller can change the endpoints a plan run is held to")
	}
}

package setup

import (
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Choosing Agentry credits records the registry signed in to: no key, no
// model to choose, its model endpoint as the base URL, its host the only
// egress, and a policy that validates. Nothing is asked.
func TestSetupRecordsAgentryCreditsAtTheSignedInRegistry(t *testing.T) {
	pol := policy.Default()
	p := &askingPrompter{scriptPrompter: scriptPrompter{answers: []string{"agentry"}}}
	if err := ConfigureModel(p, pol, ModelOpts{AgentryRegistry: "https://dev.secureagentry.net/"}); err != nil {
		t.Fatal(err)
	}
	m := pol.Model
	if m.Provider != policy.ProviderAgentry || m.Name != AgentryModelName || m.APIKeyEnv != "" || m.APIKeyKind != "" || m.APIKeySource != "" ||
		m.BaseURL != "https://dev.secureagentry.net/api/v1/x/inference/v1" || !slices.Equal(m.ProxyAllow, []string{"dev.secureagentry.net:443"}) {
		t.Fatalf("model block %+v", m)
	}
	if err := pol.Validate(); err != nil {
		t.Fatalf("the credits policy is invalid: %v", err)
	}
	if len(p.asked) != 1 {
		t.Fatalf("asked %v after the provider, want nothing", p.asked[1:])
	}
}

// Without a sign-in the provider is refused, saying to sign in; a key,
// key source, kind or base URL is refused too, since none applies.
func TestSetupRefusesAgentryCreditsWithoutASignInOrWithAKey(t *testing.T) {
	for _, c := range []struct {
		opts ModelOpts
		want string
	}{
		{ModelOpts{Provider: "agentry", Yes: true}, "agentry login"},
		{ModelOpts{Provider: "agentry", Yes: true, AgentryRegistry: "https://r.example", APIKeyEnv: "X"}, "takes no key variable"},
		{ModelOpts{Provider: "agentry", Yes: true, AgentryRegistry: "https://r.example", APIKeyKind: "api"}, "no api key kind"},
		{ModelOpts{Provider: "agentry", Yes: true, AgentryRegistry: "http://r.example"}, "must be https"},
	} {
		pol := policy.Default()
		before := pol.Model
		err := ConfigureModel(&scriptPrompter{}, pol, c.opts)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.opts, err, c.want)
		}
		if pol.Model.Provider != before.Provider {
			t.Errorf("%+v: the policy changed to %+v", c.opts, pol.Model)
		}
	}
}

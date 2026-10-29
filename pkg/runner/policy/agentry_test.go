package policy

import (
	"strings"
	"testing"
)

// A provider-agentry model names a registry's model endpoint, https except
// on this computer, and no key of any kind.
func TestAgentryModelsNameARegistrysEndpointAndNoKey(t *testing.T) {
	for _, c := range []struct {
		base, want string
	}{
		{"https://secureagentry.net/api/v1/x/inference/v1", "https://secureagentry.net"},
		{"https://dev.secureagentry.net/api/v1/x/inference/v1/", "https://dev.secureagentry.net"},
		{"http://127.0.0.1:8080/api/v1/x/inference/v1", "http://127.0.0.1:8080"},
		{"http://localhost:8080/api/v1/x/inference/v1", "http://localhost:8080"},
	} {
		if got, err := AgentryRegistry(Model{BaseURL: c.base}); err != nil || got != c.want {
			t.Errorf("%s: %q, %v", c.base, got, err)
		}
	}
	for _, bad := range []string{"", "https://secureagentry.net", "https://secureagentry.net/v1", "http://secureagentry.net/api/v1/x/inference/v1",
		"https://u:p@secureagentry.net/api/v1/x/inference/v1", "https://secureagentry.net?x=1/api/v1/x/inference/v1"} {
		if _, err := AgentryRegistry(Model{BaseURL: bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	p := Default()
	p.Model = Model{Provider: ProviderAgentry, Name: "agentry", BaseURL: "https://secureagentry.net" + AgentryInferencePath, ProxyAllow: []string{"secureagentry.net:443"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("a credits policy: %v", err)
	}
	p.Model.APIKeyEnv = "SOME_KEY"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "takes no apiKeyEnv") {
		t.Fatalf("a credits policy with a key: %v", err)
	}
}

package persistent

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// The key the person keeps for a web search provider on this computer: the
// runner policy names the provider as webSearch.provider, and its key is in
// the variable the policy names or, with the keychain as its source, in the
// keychain entry provider:<provider>, as runs and Provider settings read it.
// Seams, so a test never reads a person's policy or keychain.
var (
	// searchKeyHeld says the key is there, without reading it: the
	// variable, else the keychain's presence marker.
	searchKeyHeld = func(provider string) bool {
		w, ok := policySearchKey(provider)
		if !ok {
			return false
		}
		if strings.TrimSpace(os.Getenv(w.KeyEnv())) != "" {
			return true
		}
		if w.APIKeySource != modelkey.SourceKeyring {
			return false
		}
		present, err := modelkey.Present(provider)
		return err == nil && present
	}
	// searchKeyValue reads the key.
	searchKeyValue = func(provider string) (string, error) {
		w, ok := policySearchKey(provider)
		if !ok {
			return "", nil
		}
		return modelkey.Personal(w.KeyEnv(), w.APIKeySource, provider)
	}
)

// policySearchKey is the runner policy's webSearch block when it names
// provider.
func policySearchKey(provider string) (policy.WebSearchKey, bool) {
	path, err := policy.DefaultPath()
	if err != nil {
		return policy.WebSearchKey{}, false
	}
	pol, _, err := policy.Load(path)
	if err != nil || pol.WebSearch.Provider != provider {
		return policy.WebSearchKey{}, false
	}
	return pol.WebSearch, true
}

// searchKeyStandsIn says the key kept for o's search provider stands in for
// its secret in c: o names one, the agent stores no secret of that name,
// and no server the person added declares it, so the key would reach o
// alone. Whether the key is there is searchKeyHeld's to say.
func searchKeyStandsIn(c Config, o offer.Server) bool {
	if o.SearchKey == "" || o.Secret == "" {
		return false
	}
	if slices.ContainsFunc(c.Secrets, func(s StoredSecret) bool { return s.Name == o.Secret }) {
		return false
	}
	return !slices.ContainsFunc(c.Servers, func(srv manifest.MCPServer) bool { return slices.Contains(srv.Secrets, o.Secret) })
}

// searchKeyLine is how an offered server's consent sentence names where
// its secret comes from when a search key may stand in for it.
func searchKeyLine(o offer.Server) string {
	return fmt.Sprintf(", given your secret %s or, while you store none, the %s key you keep for web search", o.Secret, policy.WebSearchKey{Provider: o.SearchKey}.ProviderName())
}

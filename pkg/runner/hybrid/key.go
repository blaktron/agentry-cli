package hybrid

import (
	"os"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// Key is the Hybrid Analysis key as the runner policy says to read it: the
// variable its hybridAnalysis block names (HYBRID_API_KEY by default,
// where a sandbox host hands the platform's key), else, with apiKeySource
// keyring, the OS keychain entry provider:hybrid-analysis. "" is no key;
// an error is a keychain the policy names that could not be read.
func Key(pol *policy.Policy) (string, error) {
	if pol == nil {
		return strings.TrimSpace(os.Getenv(policy.DefaultHybridKeyEnv)), nil
	}
	return modelkey.Personal(pol.HybridAnalysis.KeyEnv(), pol.HybridAnalysis.APIKeySource, policy.HybridKeyProvider)
}

// FromPolicy is the client a run looks up with: the policy's key, or none,
// which asks nothing and says so.
func FromPolicy(pol *policy.Policy) (*Client, error) {
	key, err := Key(pol)
	if err != nil {
		return nil, err
	}
	return &Client{Key: key}, nil
}

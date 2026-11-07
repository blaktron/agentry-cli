package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// runContextWindow settles the model's context window for one run. A model on
// an endpoint of the person's own whose policy records no window is asked for
// it, with the probe `agentry setup` makes (probeContextWindow) and within its
// budget; what the endpoint reports is set on pol for this run alone, and the
// policy file is never written. It says what it found, or that it found none,
// on w, and returns where the window came from for the plan: the endpoint's
// URL, or "" when the run keeps the policy's window (or has none).
//
// The key goes to the endpoint only as a bearer, and only when the run has
// one: the variable the policy names, else the keychain entry it names, read
// through the run's own memoised lookup so the run's resolution later reads
// the same value without asking again.
func runContextWindow(ctx context.Context, w io.Writer, pol *policy.Policy) string {
	m := pol.Model
	if m.ContextWindow != 0 || !m.OwnEndpoint() || strings.TrimSpace(m.Name) == "" {
		return ""
	}
	key := strings.TrimSpace(os.Getenv(m.APIKeyEnv))
	if key == "" && m.APIKeySource == modelkey.SourceKeyring {
		// A keychain that cannot be read leaves the probe keyless; the run's
		// own resolution meets the same answer and says so.
		if v, err := engine.KeyringValue(modelkey.Account(m.Provider)); err == nil {
			key = strings.TrimSpace(v)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, setup.ContextProbeBudget)
	defer cancel()
	found, err := probeContextWindow(ctx, m.BaseURL, m.Name, key)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(w, "context window of %s: could not ask the endpoint: %v\n", m.Name, err)
	case found.Tokens > 0 && policy.CheckContextWindow(found.Tokens) == nil:
		pol.Model.ContextWindow = found.Tokens
		_, _ = fmt.Fprintf(w, "context window of %s: %d tokens, as %s reports it (for this run; `agentry setup` records it in the policy)\n", m.Name, found.Tokens, found.Source)
		return found.Source
	case found.Tokens > 0:
		_, _ = fmt.Fprintf(w, "context window of %s: not taken: %s reports %d tokens, outside %d to %d\n", m.Name, found.Source, found.Tokens, policy.MinContextWindow, policy.MaxContextWindow)
	}
	_, _ = fmt.Fprintf(w, "context window of %s: unknown, the endpoint did not report one; the run goes on with the limits for an unknown window (record it with `agentry setup --context-window`)\n", m.Name)
	return ""
}

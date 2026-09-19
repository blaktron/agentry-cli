// Package setup implements the operator-experience flows of RUNNER-PLAN-2:
// first-run machine setup with LLM-provider configuration (§P1) and the
// guided MCP-profile setup that collects a BriefAgent's keys and
// identities (§P3). Everything works through the Prompter interface so the
// CLI (a TTY) and the desktop app (a form) drive the same logic — no
// parallel implementations (§P8).
package setup

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"
)

// Prompter is the interaction surface guided flows run through.
type Prompter interface {
	// Ask shows a prompt with a default and returns the answer ("" keeps
	// the default).
	Ask(prompt, def string) (string, error)
	// AskSecret reads a value without echoing it. An empty answer means
	// "leave unset" and is not an error.
	AskSecret(prompt string) (string, error)
	// Confirm asks a yes/no question with a default.
	Confirm(prompt string, def bool) (bool, error)
	// Say emits informational text.
	Say(text string)
}

// ProviderDefault is one row of the §P1 provider table.
type ProviderDefault struct {
	Provider   string
	Model      string
	APIKeyEnv  string
	ProxyAllow []string
}

// providerDefaults is the closed §P1 table; "custom" is handled by asking.
var providerDefaults = []ProviderDefault{
	{Provider: "anthropic", Model: "claude-sonnet-4-5", APIKeyEnv: "ANTHROPIC_API_KEY", ProxyAllow: []string{"api.anthropic.com:443"}},
	{Provider: "openai", Model: "gpt-5.2", APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{"api.openai.com:443"}},
	{Provider: "google", Model: "gemini-3-pro", APIKeyEnv: "GEMINI_API_KEY", ProxyAllow: []string{"generativelanguage.googleapis.com:443"}},
}

// Providers lists the selectable provider names (custom last).
func Providers() []string {
	out := make([]string, 0, len(providerDefaults)+1)
	for _, d := range providerDefaults {
		out = append(out, d.Provider)
	}
	return append(out, "custom")
}

// DefaultsFor returns the table row for a provider (nil for custom or
// unknown).
func DefaultsFor(provider string) *ProviderDefault {
	for i := range providerDefaults {
		if providerDefaults[i].Provider == provider {
			return &providerDefaults[i]
		}
	}
	return nil
}

// ModelOpts carries non-interactive answers (flags / desktop form); empty
// fields are asked for or defaulted.
type ModelOpts struct {
	Provider  string
	Model     string
	APIKeyEnv string
	// APIKeySource is "" (keep what the policy has for the same provider,
	// else env), "env" or "keyring": where the key comes from when the
	// variable is unset (agentry-cli#148). Interactive setup offers to
	// store the key in the keychain and sets keyring itself.
	APIKeySource string
	// StoreKey puts a key into the OS keychain (nil: modelkey.Store). A
	// seam: tests have no keychain, and a form-driven caller stores the
	// key itself before calling.
	StoreKey func(provider, key string) error
	BaseURL  string
	// Yes suppresses prompting entirely: table defaults fill the gaps and
	// a custom provider with no BaseURL is an error rather than a question.
	Yes bool
}

// ConfigureModel resolves the operator's LLM provider preference (req 5)
// into the policy's model block, prompting through p where opts leave
// gaps. Everything outside `model:` is untouched.
func ConfigureModel(p Prompter, pol *policy.Policy, opts ModelOpts) error {
	prov := strings.ToLower(strings.TrimSpace(opts.Provider))
	if prov == "" {
		if opts.Yes {
			prov = pol.Model.Provider
			if prov == "" {
				prov = "anthropic"
			}
		} else {
			ans, err := p.Ask(fmt.Sprintf("LLM provider for BriefAgent runs (%s)", strings.Join(Providers(), " | ")), defaultOr(pol.Model.Provider, "anthropic"))
			if err != nil {
				return err
			}
			prov = strings.ToLower(strings.TrimSpace(ans))
		}
	}

	d := DefaultsFor(prov)
	if d == nil && prov != "custom" {
		// An unlisted provider name is treated as custom-with-a-name:
		// docker-agent accepts many providers; the table only carries the
		// ones whose endpoints we can derive.
		p.Say(fmt.Sprintf("provider %q is not in the built-in table; treating it as a custom/OpenAI-compatible endpoint", prov))
	}

	// A provider switch prefers the new provider's table defaults; staying
	// on the same provider preserves whatever the operator had customized.
	sameProvider := prov == pol.Model.Provider

	model := opts.Model
	if model == "" {
		def := ""
		switch {
		case sameProvider && pol.Model.Name != "":
			def = pol.Model.Name
		case d != nil:
			def = d.Model
		}
		if opts.Yes {
			model = def
		} else {
			ans, err := p.Ask("Model", def)
			if err != nil {
				return err
			}
			model = strings.TrimSpace(ans)
		}
	}
	if model == "" {
		return fmt.Errorf("no model chosen for provider %q (pass --model)", prov)
	}

	keyEnv := opts.APIKeyEnv
	if keyEnv == "" {
		def := ""
		switch {
		case sameProvider && pol.Model.APIKeyEnv != "":
			def = pol.Model.APIKeyEnv
		case d != nil:
			def = d.APIKeyEnv
		}
		if def == "" {
			def = strings.ToUpper(prov) + "_API_KEY"
		}
		if opts.Yes {
			keyEnv = def
		} else {
			ans, err := p.Ask("Environment variable holding YOUR api key (the runner never stores key values)", def)
			if err != nil {
				return err
			}
			keyEnv = strings.TrimSpace(ans)
		}
	}

	baseURL := strings.TrimSpace(opts.BaseURL)
	var proxyAllow []string
	switch {
	case d != nil && baseURL == "":
		proxyAllow = append([]string(nil), d.ProxyAllow...)
	default:
		if baseURL == "" {
			if opts.Yes {
				return fmt.Errorf("provider %q needs --base-url so the egress proxy allowlist can be derived", prov)
			}
			ans, err := p.Ask("Endpoint base URL (e.g. https://llm.internal:8443/v1)", pol.Model.BaseURL)
			if err != nil {
				return err
			}
			baseURL = strings.TrimSpace(ans)
		}
		hp, err := hostPortOf(baseURL)
		if err != nil {
			return err
		}
		proxyAllow = []string{hp}
	}

	// Where the key comes from when the variable is unset. A form-driven
	// caller says so; on a terminal the offer is to store it now, for the
	// runs launched from somewhere with no shell environment.
	source := opts.APIKeySource
	if source == "" && sameProvider {
		source = pol.Model.APIKeySource
	}
	if source != "" && source != modelkey.SourceEnv && source != modelkey.SourceKeyring {
		return fmt.Errorf("api key source %q must be env or keyring", source)
	}
	if !opts.Yes && opts.APIKeySource == "" && source != modelkey.SourceKeyring && !modelkey.Disabled() {
		store, err := p.Confirm(fmt.Sprintf("Store the key in this machine's keychain now, for runs launched without $%s (the variable still wins when it is set)?", keyEnv), false)
		if err != nil {
			return err
		}
		if store {
			key, err := p.AskSecret(fmt.Sprintf("API key for %s (goes to the OS keychain, never to the policy file)", prov))
			if err != nil {
				return err
			}
			if strings.TrimSpace(key) == "" {
				p.Say("nothing stored: the key was empty")
			} else {
				storeKey := opts.StoreKey
				if storeKey == nil {
					storeKey = modelkey.Store
				}
				// A keychain that cannot be reached is not a reason to
				// leave the policy unwritten: the variable path is as it
				// was, and the reason is said.
				if err := storeKey(prov, key); err != nil {
					p.Say(fmt.Sprintf("not stored — %v; the key stays yours to set as $%s", err, keyEnv))
				} else {
					source = modelkey.SourceKeyring
					p.Say(fmt.Sprintf("stored in the OS keychain as %s", modelkey.Account(prov)))
				}
			}
		}
	}
	if source == modelkey.SourceEnv {
		source = "" // the default, unwritten
	}

	pol.Model = policy.Model{
		Provider:     prov,
		Name:         model,
		APIKeyEnv:    keyEnv,
		APIKeySource: source,
		BaseURL:      baseURL,
		ProxyAllow:   proxyAllow,
	}
	from := "$" + keyEnv
	if source == modelkey.SourceKeyring {
		from += ", else the OS keychain"
	}
	p.Say(fmt.Sprintf("model: %s/%s (key from %s; agent-container egress limited to %s)",
		prov, model, from, strings.Join(proxyAllow, ", ")))
	return nil
}

// hostPortOf derives the proxy allowlist entry from a base URL — the
// agent container's ONLY egress (RUNNER-PLAN §R4), so it must be exact.
func hostPortOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("base URL %q must be a full http(s) URL", raw)
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		default:
			port = "443"
		}
	}
	return host + ":" + port, nil
}

// GuidedProfileOpts parameterizes the §P3 flow.
type GuidedProfileOpts struct {
	Ref    string
	Digest string
	// CapabilityLines is the contract display (what the keys are FOR).
	CapabilityLines []string
	// Secrets are the manifest-declared env names to collect.
	Secrets []string
	// DefaultKeep seeds the keep question (policy default or flag).
	DefaultKeep bool
	// KeepDecided suppresses the keep question (the flag already chose).
	KeepDecided bool
}

// GuidedProfile runs the §P3 conversation and returns the new profile.
// Empty secret answers are recorded as absent (resolve from the
// environment at run time), not as empty strings.
func GuidedProfile(p Prompter, opts GuidedProfileOpts) (*profile.Profile, error) {
	p.Say(fmt.Sprintf("No MCP profile exists for %s (%s) — guided setup configures its keys and identities once.",
		opts.Ref, shortDigest(opts.Digest)))
	if len(opts.CapabilityLines) > 0 {
		p.Say("Declared contract:")
		for _, line := range opts.CapabilityLines {
			p.Say("  " + line)
		}
	}
	secrets := map[string]string{}
	names := append([]string(nil), opts.Secrets...)
	sort.Strings(names)
	for _, name := range names {
		v, err := p.AskSecret(fmt.Sprintf("Value for %s (empty = resolve from the environment at run time)", name))
		if err != nil {
			return nil, err
		}
		if v != "" {
			secrets[name] = v
		}
	}
	keep := opts.DefaultKeep
	if !opts.KeepDecided {
		var err error
		keep, err = p.Confirm("Keep this MCP profile for future runs of this exact version?", opts.DefaultKeep)
		if err != nil {
			return nil, err
		}
	}
	prof := profile.New(opts.Ref, opts.Digest, keep, secrets)
	if keep {
		p.Say("Profile will be kept after the run (remove later with `agentry profiles remove`).")
	} else {
		p.Say("Profile will be removed when the run completes.")
	}
	return prof, nil
}

func shortDigest(d string) string {
	s := strings.TrimPrefix(d, "sha256:")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

func defaultOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

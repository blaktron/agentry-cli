// Package setup implements the operator-experience flows of RUNNER-PLAN-2:
// first-run machine setup with LLM-provider configuration (§P1), the
// guided MCP-profile setup that collects a QuickAgent's keys and
// identities (§P3), and the model catalog — one read of a provider's
// models listing with the operator's key, so a form can offer the models
// rather than ask for one to be typed, which is also the endpoint test the
// form gets for free (agentry-cli#170; models.go). Everything works
// through the Prompter interface so the CLI (a TTY) and the desktop app (a
// form) drive the same logic — no parallel implementations (§P8); the
// provider table below is the one place that knows a provider's endpoints
// and how its key is sent.
package setup

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/profile"

	"github.com/blaktron/agentry-cli/pkg/termtext"
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
	// ModelsURL is the provider's models listing; KeyHeader the header
	// its key rides in (keyHeaderBearer is "Authorization: Bearer").
	ModelsURL string
	KeyHeader string
}

// keyHeaderBearer marks a provider whose key is an Authorization bearer
// rather than a header of its own.
const keyHeaderBearer = "Authorization"

// providerDefaults is the closed §P1 table; "custom" is handled by asking
// (its endpoint is the operator's base URL, its key a bearer, the OpenAI
// shape every compatible endpoint uses).
var providerDefaults = []ProviderDefault{
	{Provider: "anthropic", Model: "claude-sonnet-4-5", APIKeyEnv: "ANTHROPIC_API_KEY", ProxyAllow: []string{"api.anthropic.com:443"},
		ModelsURL: "https://api.anthropic.com/v1/models", KeyHeader: "x-api-key"},
	{Provider: "openai", Model: "gpt-5.2", APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{"api.openai.com:443"},
		ModelsURL: "https://api.openai.com/v1/models", KeyHeader: keyHeaderBearer},
	{Provider: "google", Model: "gemini-3-pro", APIKeyEnv: "GEMINI_API_KEY", ProxyAllow: []string{"generativelanguage.googleapis.com:443"},
		ModelsURL: "https://generativelanguage.googleapis.com/v1beta/models", KeyHeader: "x-goog-api-key"},
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
	// APIKeyKind is what the credential is (policy apiKeyKind,
	// agentry-cli#171): "" (keep what the policy has for the same
	// provider, else api), "api" or "subscription". A form-driven caller
	// that stored the key itself passes policy.CredentialKindOf's answer;
	// an interactive setup derives it from the key it stores; a
	// variable-only setup takes --api-key-kind.
	APIKeyKind string
	BaseURL    string
	// ChatGPT offers Sign in with ChatGPT for provider openai (plan
	// model-sign-ins.md §4.3; agentry-cli#739): nil offers none. APIKeyKind
	// chatgpt-plan asks for it without the question.
	ChatGPT ChatGPTOffer
	// Claude offers ClaudeSignInLabel for provider anthropic (plan
	// model-sign-ins.md §4.4; agentry-cli#740): nil offers none.
	// APIKeyKind claude-signin asks for it without the question.
	Claude ClaudeOffer
	// Yes suppresses prompting entirely: table defaults fill the gaps, the
	// same provider's policy fills what it customized, and a custom
	// provider with no BaseURL left is an error rather than a question.
	Yes bool
}

// ChatGPTOffer is how setup reaches the ChatGPT sign-in, which the caller
// owns (the store, the browser, the network).
type ChatGPTOffer struct {
	// Kept is the account of a kept ChatGPT sign-in that still works, ""
	// when there is none.
	Kept func() string
	// SignIn signs in (fresh, or when none is kept) or refreshes the kept
	// sign-in, and returns its account and the models its plan offers.
	SignIn func(fresh bool) (account string, models []string, err error)
}

// ClaudeSignInLabel is the Claude choice's wording everywhere (plan D24):
// plain text that runs use Claude Code, no logo, no "Sign in with Claude".
const ClaudeSignInLabel = "Use your Claude subscription (runs Claude Code)"

// AnthropicKeyCredits is what the API-key choice for Anthropic says (D24):
// the plans whose included API credits a key spends.
const AnthropicKeyCredits = "an Anthropic API key (the Max and Team plans include monthly API credits, spent through a key from the Console organization linked to the plan)"

// ClaudeOffer is how setup reaches Claude Code's own sign-in, which the
// caller owns (the image, docker, the terminal).
type ClaudeOffer struct {
	// Kept is the account of the Claude Code image's kept sign-in, ""
	// when there is none.
	Kept func() string
	// SignIn runs Claude Code's own sign-in and returns its account.
	SignIn func() (account string, err error)
}

// configureClaude is the model setup for a Claude subscription through
// Claude Code: Claude Code's own sign-in, a model, and the policy's model
// block. No key variable and no base URL: Claude Code talks to Anthropic
// itself, and the proxy allows Anthropic's API and the host Claude Code
// refreshes its sign-in at, alone (invariant I2).
func configureClaude(p Prompter, pol *policy.Policy, opts ModelOpts) error {
	if opts.Claude.SignIn == nil {
		return errors.New("this setup cannot start Claude Code's sign-in")
	}
	acct := ""
	if opts.Claude.Kept != nil {
		acct = opts.Claude.Kept()
	}
	fresh := acct == ""
	if !fresh && !opts.Yes {
		use, err := p.Confirm(fmt.Sprintf("Use the Claude sign-in kept on this computer (%s)?", acct), true)
		if err != nil {
			return err
		}
		fresh = !use
	}
	if fresh {
		p.Say(ClaudeSignInLabel + ": Claude Code signs you in itself; your Claude plan pays for the runs on this computer, and hosted runs never use it.")
		var err error
		if acct, err = opts.Claude.SignIn(); err != nil {
			return err
		}
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		def := DefaultsFor(policy.ProviderAnthropic).Model
		if pol.Model.APIKeyKind == policy.KeyKindClaudeSignIn && pol.Model.Name != "" {
			def = pol.Model.Name
		}
		if opts.Yes {
			model = def
		} else {
			ans, err := p.Ask("Model (a Claude model id, or Claude Code's alias: sonnet, opus)", def)
			if err != nil {
				return err
			}
			model = strings.TrimSpace(ans)
		}
	}
	if model == "" {
		return errors.New("no model chosen for the Claude subscription (pass --model)")
	}
	pol.Model = policy.Model{
		Provider:   policy.ProviderAnthropic,
		Name:       model,
		APIKeyKind: policy.KeyKindClaudeSignIn,
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn),
	}
	who := ""
	if acct != "" {
		who = " as " + acct
	}
	p.Say(fmt.Sprintf("model: anthropic/%s, paid by your Claude subscription (signed in%s; runs use Claude Code, and the agent container reaches %s alone)",
		model, who, strings.Join(pol.Model.ProxyAllow, ", ")))
	return nil
}

// configureChatGPT is the model setup for a ChatGPT plan: the sign-in, the
// model from the plan's own list, and the policy's model block. No key
// variable, no base URL: the plan's endpoint is OpenAI's, and the proxy
// allows it alone (invariant I2).
func configureChatGPT(p Prompter, pol *policy.Policy, opts ModelOpts) error {
	if opts.ChatGPT.SignIn == nil {
		return errors.New("this setup cannot sign in with ChatGPT")
	}
	fresh := true
	if acct := keptChatGPT(opts); acct != "" {
		fresh = false
		if !opts.Yes {
			use, err := p.Confirm(fmt.Sprintf("Use the ChatGPT sign-in kept on this computer (%s)?", acct), true)
			if err != nil {
				return err
			}
			fresh = !use
		}
	}
	if fresh {
		p.Say("Signing in with ChatGPT in your browser. Your ChatGPT plan pays for the runs on this computer; hosted runs never use it.")
	}
	acct, models, err := opts.ChatGPT.SignIn(fresh)
	if err != nil {
		return err
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		def := ""
		switch {
		case pol.Model.APIKeyKind == policy.KeyKindChatGPTPlan && slices.Contains(models, pol.Model.Name):
			def = pol.Model.Name
		case len(models) > 0:
			def = models[0]
		}
		if opts.Yes {
			model = def
		} else {
			q := "Model"
			if len(models) > 0 {
				q += " (your plan offers " + strings.Join(models, ", ") + ")"
			}
			ans, err := p.Ask(q, def)
			if err != nil {
				return err
			}
			model = strings.TrimSpace(ans)
		}
	}
	if model == "" {
		return errors.New("no model chosen for the ChatGPT plan (pass --model)")
	}
	if len(models) > 0 && !slices.Contains(models, model) {
		p.Say(fmt.Sprintf("warning: your plan's model list does not name %q; runs will ask for it anyway", model))
	}
	pol.Model = policy.Model{
		Provider:   policy.ProviderOpenAI,
		Name:       model,
		APIKeyKind: policy.KeyKindChatGPTPlan,
		// The one endpoint the compiler holds a plan run to (I2).
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindChatGPTPlan),
	}
	who := ""
	if acct != "" {
		who = " as " + acct
	}
	p.Say(fmt.Sprintf("model: openai/%s, paid by your ChatGPT plan (signed in%s; runs use Codex, and the agent container reaches %s alone)",
		model, who, strings.Join(pol.Model.ProxyAllow, ", ")))
	return nil
}

func keptChatGPT(opts ModelOpts) string {
	if opts.ChatGPT.Kept == nil {
		return ""
	}
	return opts.ChatGPT.Kept()
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
			ans, err := p.Ask(fmt.Sprintf("LLM provider for QuickAgent runs (%s)", strings.Join(Providers(), " | ")), defaultOr(pol.Model.Provider, "anthropic"))
			if err != nil {
				return err
			}
			prov = strings.ToLower(strings.TrimSpace(ans))
		}
	}

	d := DefaultsFor(prov)
	if d == nil && prov != "custom" {
		// An unlisted provider name is passed to the harness as it is:
		// docker-agent knows more providers than the table, which only
		// carries the ones whose endpoints we can derive — but it does
		// not know "custom", which the adapter renders as its openai
		// client at the base URL (agentry-cli#169), and it will not know
		// a name made up here.
		p.Say(fmt.Sprintf("provider %q is not in the built-in table; the harness must know it by that name — for an OpenAI-compatible endpoint of your own, use custom with --base-url", prov))
	}

	// OpenAI is paid by a key or by the person's ChatGPT plan (D15, D16):
	// asked when setup can sign in, unless the caller already said.
	if prov == policy.ProviderOpenAI {
		kind := strings.ToLower(strings.TrimSpace(opts.APIKeyKind))
		if kind == "" && !opts.Yes && opts.ChatGPT.SignIn != nil {
			def := "key"
			if pol.Model.APIKeyKind == policy.KeyKindChatGPTPlan {
				def = "chatgpt"
			}
			ans, err := p.Ask("Pay for OpenAI models with (chatgpt = sign in with ChatGPT and use your plan | key = an OpenAI API key)", def)
			if err != nil {
				return err
			}
			switch strings.ToLower(strings.TrimSpace(ans)) {
			case "chatgpt", "chatgpt-plan", "plan":
				kind = policy.KeyKindChatGPTPlan
			case "key", "api", "":
				kind = policy.KeyKindAPI
				opts.APIKeyKind = kind
			default:
				return fmt.Errorf("%q is neither chatgpt nor key", ans)
			}
		}
		if kind == policy.KeyKindChatGPTPlan {
			return configureChatGPT(p, pol, opts)
		}
	} else if strings.EqualFold(strings.TrimSpace(opts.APIKeyKind), policy.KeyKindChatGPTPlan) {
		return fmt.Errorf("api key kind chatgpt-plan needs provider openai (it is %q)", prov)
	}

	// Anthropic is paid by a key, a pasted subscription token (DR-26), or
	// the person's Claude subscription through Claude Code's own sign-in
	// (D19–D24): the last asked when setup can start it.
	if prov == policy.ProviderAnthropic {
		kind := strings.ToLower(strings.TrimSpace(opts.APIKeyKind))
		if kind == "" && !opts.Yes && opts.Claude.SignIn != nil {
			def := "key"
			if pol.Model.APIKeyKind == policy.KeyKindClaudeSignIn {
				def = "claude"
			}
			ans, err := p.Ask("Pay for Anthropic models with (claude = "+ClaudeSignInLabel+" | key = "+AnthropicKeyCredits+")", def)
			if err != nil {
				return err
			}
			switch strings.ToLower(strings.TrimSpace(ans)) {
			case "claude", policy.KeyKindClaudeSignIn, "subscription-signin":
				kind = policy.KeyKindClaudeSignIn
			case "key", "api", "":
			default:
				return fmt.Errorf("%q is neither claude nor key", ans)
			}
		}
		if kind == policy.KeyKindClaudeSignIn {
			return configureClaude(p, pol, opts)
		}
	} else if strings.EqualFold(strings.TrimSpace(opts.APIKeyKind), policy.KeyKindClaudeSignIn) {
		return fmt.Errorf("api key kind claude-signin needs provider anthropic (it is %q)", prov)
	}

	// A provider switch prefers the new provider's table defaults; staying
	// on the same provider preserves whatever the operator had customized.
	// Leaving a ChatGPT plan for a key keeps none of the plan's setup.
	sameProvider := prov == pol.Model.Provider && !policy.IsModelSignIn(pol.Model.APIKeyKind)

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
			// The same-provider rule covers the URL too: staying on
			// custom keeps the endpoint the policy has — the question's
			// default, and under --yes the answer — and derives the
			// allowlist from it again (agentry-cli#188). A switch brings
			// no URL along.
			if opts.Yes {
				if sameProvider {
					baseURL = strings.TrimSpace(pol.Model.BaseURL)
				}
				if baseURL == "" {
					return fmt.Errorf("provider %q needs --base-url so the egress proxy allowlist can be derived", prov)
				}
			} else {
				ans, err := p.Ask("Endpoint base URL (e.g. https://llm.internal:8443/v1)", pol.Model.BaseURL)
				if err != nil {
					return err
				}
				baseURL = strings.TrimSpace(ans)
			}
		}
		hp, err := hostPortOf(baseURL)
		if err != nil {
			return err
		}
		proxyAllow = []string{hp}
		// Plain http off this machine is allowed and said out loud
		// (agentry-cli#377): the key rides every model call.
		if w := PlainHTTPWarning(baseURL); w != "" {
			p.Say("warning: " + w)
		}
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
	kind := strings.ToLower(strings.TrimSpace(opts.APIKeyKind))
	if kind == "" && sameProvider {
		kind = pol.Model.APIKeyKind
	}
	if kind != "" && kind != policy.KeyKindAPI && kind != policy.KeyKindSubscription {
		return fmt.Errorf("api key kind %q must be api or subscription", kind)
	}
	if kind == policy.KeyKindSubscription && prov != policy.ProviderAnthropic {
		return fmt.Errorf("api key kind subscription needs provider anthropic (it is %q): %s", prov, policy.SubscriptionTokenSentence)
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
					// The kind is read from the value at the one moment
					// this flow holds it.
					kind = policy.CredentialKindOf(prov, key)
				}
			}
		}
	}
	if source == modelkey.SourceEnv {
		source = "" // the default, unwritten
	}
	if kind == policy.KeyKindAPI {
		kind = "" // the default, unwritten
	}

	pol.Model = policy.Model{
		Provider:     prov,
		Name:         model,
		APIKeyEnv:    keyEnv,
		APIKeySource: source,
		APIKeyKind:   kind,
		BaseURL:      baseURL,
		ProxyAllow:   proxyAllow,
	}
	from := "$" + keyEnv
	if source == modelkey.SourceKeyring {
		from += ", else the OS keychain"
	}
	p.Say(fmt.Sprintf("model: %s/%s (key from %s; agent-container egress limited to %s)",
		prov, model, from, strings.Join(proxyAllow, ", ")))
	if kind == policy.KeyKindSubscription {
		// The shared sentence stands alone (agentry-cli#182); a prefix
		// made it ungrammatical (agentry-cli#183).
		p.Say(policy.SubscriptionTokenSentence)
	}
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
	return net.JoinHostPort(host, port), nil
}

// GuidedProfileOpts parameterizes the §P3 flow.
type GuidedProfileOpts struct {
	Ref    string
	Digest string
	// CapabilityLines is the contract display (what the keys are FOR).
	CapabilityLines []string
	// Secrets are the manifest-declared env names to collect.
	Secrets []string
	// Optional are the ones a run may go without (the manifest's
	// optionalSecrets, DR-70): asked the same way, and said so.
	Optional []string
	// DefaultKeep seeds the keep question (policy default or flag).
	DefaultKeep bool
	// KeepDecided suppresses the keep question (the flag already chose).
	KeepDecided bool
	// Help is the release guide's connect help per secret name (DEV-141):
	// where to create the key, which scopes it needs, in markdown that may
	// span lines (DEV-167). Printed as text before the question for that
	// key, beneath its name behind termtext's gutter when it spans lines.
	Help map[string]string
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
		if h := strings.TrimSpace(opts.Help[name]); h != "" {
			// The help is markdown that may span lines (DEV-167), so it is
			// said beneath the key's name behind the gutter: no line of it
			// can read as the CLI's own (another key's prompt, a contract
			// line). A one-line help is said beside the name, as before.
			if strings.Contains(h, "\n") {
				p.Say(name + ":\n" + termtext.Quote(h))
			} else {
				p.Say(name + ": " + termtext.Safe(h))
			}
		}
		q := fmt.Sprintf("Value for %s (empty = resolve from the environment at run time)", name)
		if slices.Contains(opts.Optional, name) {
			q = fmt.Sprintf("Value for %s, optional (empty = resolve from the environment at run time, or the run goes without it)", name)
		}
		v, err := p.AskSecret(q)
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

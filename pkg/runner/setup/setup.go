// Package setup implements the operator-experience flows: first-run machine
// setup with LLM-provider configuration, the guided MCP-profile setup that
// collects a QuickAgent's keys and identities, and the model catalog — one read
// of a provider's models listing with the operator's key, so a form can offer
// the models rather than ask for one to be typed, which is also the endpoint
// test the form gets for free (models.go). Everything works through the
// Prompter interface so the CLI (a TTY) and the desktop app (a form) drive the
// same logic — no parallel implementations; the provider table below is the one
// place that knows a provider's endpoints and how its key is sent.
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

// ProviderDefault is one row of the provider table.
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

// providerDefaults is the closed table; "custom" is handled by asking
// (its endpoint is the operator's base URL, its key a bearer, the OpenAI
// shape every compatible endpoint uses).
var providerDefaults = []ProviderDefault{
	{Provider: "anthropic", Model: "claude-sonnet-4-5", APIKeyEnv: "ANTHROPIC_API_KEY", ProxyAllow: []string{"api.anthropic.com:443"},
		ModelsURL: "https://api.anthropic.com/v1/models", KeyHeader: "x-api-key"},
	{Provider: "openai", Model: "gpt-5.2", APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{"api.openai.com:443"},
		ModelsURL: "https://api.openai.com/v1/models", KeyHeader: keyHeaderBearer},
	{Provider: "google", Model: "gemini-3-pro", APIKeyEnv: "GEMINI_API_KEY", ProxyAllow: []string{"generativelanguage.googleapis.com:443"},
		ModelsURL: "https://generativelanguage.googleapis.com/v1beta/models", KeyHeader: "x-goog-api-key"},
	// The providers whose account a person may connect, each also paid by a
	// pasted key. No default model and no model list: the person types the
	// model id. The key variables are the ones docker-agent's aliases read
	// (Poe's is the named provider's token_key), and the endpoints
	// docker-agent's.
	{Provider: policy.ProviderOpenRouter, APIKeyEnv: "OPENROUTER_API_KEY", ProxyAllow: []string{"openrouter.ai:443"}},
	{Provider: policy.ProviderPoe, APIKeyEnv: "POE_API_KEY", ProxyAllow: []string{"api.poe.com:443"}},
	{Provider: policy.ProviderHuggingFace, APIKeyEnv: "HF_TOKEN", ProxyAllow: []string{"router.huggingface.co:443"}},
	// Vultr Serverless Inference: an OpenAI-compatible endpoint paid by a key
	// from the person's Vultr account. No default model, because Vultr's
	// catalogue changes; its listing offers the models a run can use.
	{Provider: policy.ProviderVultr, APIKeyEnv: "VULTR_INFERENCE_API_KEY", ProxyAllow: []string{"api.vultrinference.com:443"},
		ModelsURL: "https://api.vultrinference.com/v1/models", KeyHeader: keyHeaderBearer},
}

// Providers lists the selectable provider names (agentry, paid with
// Agentry credits, and copilot, paid by a Copilot plan alone, then custom
// last).
func Providers() []string {
	out := make([]string, 0, len(providerDefaults)+3)
	for _, d := range providerDefaults {
		out = append(out, d.Provider)
	}
	return append(out, policy.ProviderAgentry, policy.ProviderCopilot, "custom")
}

// AgentryModelName is the model name a provider-agentry policy records:
// the registry forwards every request to the model its staff chose, so the
// name the harness sends is only a label.
const AgentryModelName = "agentry"

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
	// APIKeySource is "" (keep what the policy has for the same provider, else
	// env), "env" or "keyring": where the key comes from when the variable is
	// unset. Interactive setup offers to store the key in the keychain and sets
	// keyring itself.
	APIKeySource string
	// StoreKey puts a key into the OS keychain (nil: modelkey.Store). A
	// seam: tests have no keychain, and a form-driven caller stores the
	// key itself before calling.
	StoreKey func(provider, key string) error
	// APIKeyKind is what the credential is (policy apiKeyKind): "" (keep what
	// the policy has for the same provider, else api), "api" or "subscription".
	// A form-driven caller that stored the key itself passes
	// policy.CredentialKindOf's answer; an interactive setup derives it from
	// the key it stores; a variable-only setup takes --api-key-kind.
	APIKeyKind string
	BaseURL    string
	// ChatGPT offers Sign in with ChatGPT for provider openai: nil offers none.
	// APIKeyKind chatgpt-plan asks for it without the question.
	ChatGPT ChatGPTOffer
	// Claude offers ClaudeSignInLabel for provider anthropic: nil offers none.
	// APIKeyKind claude-signin asks for it without the question.
	Claude ClaudeOffer
	// Copilot offers Sign in with GitHub for provider copilot, its one way
	// to pay.
	Copilot CopilotOffer
	// AgentryRegistry is the SecureAgentry registry this computer is signed
	// in to, which provider agentry pays through with the account's
	// credits; "" when it is signed in to none, and then that provider is
	// refused.
	AgentryRegistry string
	// Connect offers Connect your account for OpenRouter, Poe and Hugging Face:
	// nil offers none. ConnectAccount asks for it without the question.
	Connect        ConnectOffer
	ConnectAccount bool
	// CopilotPlan is the Copilot plan the account is on (policy.CopilotPlans:
	// individual, business, enterprise), which names the Copilot API host
	// runs reach; "" keeps the policy's, else asks, else individual.
	CopilotPlan string
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

// ClaudeSignInLabel is the Claude choice's wording everywhere:
// plain text that runs use Claude Code, no logo, no "Sign in with Claude".
const ClaudeSignInLabel = "Use your Claude subscription (runs Claude Code)"

// AnthropicKeyCredits is what the API-key choice for Anthropic says:
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

// CopilotOffer is how setup reaches the Copilot sign-in, which the caller
// owns (the store, the browser, the network).
type CopilotOffer struct {
	// Kept is the account of a kept Copilot sign-in that still works, ""
	// when there is none.
	Kept func() string
	// SignIn signs in with GitHub and returns the account.
	SignIn func() (account string, err error)
	// Models lists the model ids the Copilot plan named offers the signed-in
	// account, asked of Copilot CLI, in its order; nil lists none. Setup asks
	// it only when it is about to ask for the model.
	Models func(plan string) ([]string, error)
}

// ConnectLabel is the connect choice's wording.
const ConnectLabel = "Connect your account"

// ConnectOffer is how setup reaches a connect, which the caller owns (the
// browser, the network, the keychain). Each takes the policy's provider.
type ConnectOffer struct {
	// Kept is how a kept connect for provider is named ("Connected as …"),
	// "" when none is kept.
	Kept func(provider string) string
	// Connect connects the person's account in their browser, keeps the
	// key it minted in the OS keychain, and returns how the account is
	// named.
	Connect func(provider string) (account string, err error)
	// Forget forgets the connect kept for provider once a pasted key has
	// replaced its key in the keychain, and says what became of it (""
	// when none was kept).
	Forget func(provider string) (outcome string, err error)
}

// connectNames are the connect providers, as a person knows them, with a
// model id of the shape each takes.
var connectNames = map[string]struct{ name, example string }{
	policy.ProviderOpenRouter:  {"OpenRouter", "anthropic/claude-sonnet-4.5"},
	policy.ProviderPoe:         {"Poe", "Claude-Sonnet-4.6"},
	policy.ProviderHuggingFace: {"Hugging Face", "openai/gpt-oss-120b:groq, a model and the provider that serves it"},
}

// IsConnectProvider reports whether provider's account can be connected.
func IsConnectProvider(provider string) bool { _, ok := connectNames[provider]; return ok }

// configureConnect is the model setup for a connected account: the
// connect (or the one kept), the model id the person types, and a
// policy whose key is the provider's keychain entry, where the connect
// kept it. The variable still wins when it is set.
func configureConnect(p Prompter, pol *policy.Policy, opts ModelOpts, prov string) error {
	if opts.Connect.Connect == nil {
		return errors.New("this setup cannot connect an account")
	}
	if modelkey.Disabled() {
		return fmt.Errorf("%s keeps the key in the OS keychain, which is disabled here (%s is set): paste a key into the variable instead", ConnectLabel, modelkey.EnvNoKeyring)
	}
	d, n := DefaultsFor(prov), connectNames[prov]
	same := pol.Model.Provider == prov
	if opts.Yes && strings.TrimSpace(opts.Model) == "" && (!same || pol.Model.Name == "") {
		// Said before the browser opens, not after the account is connected.
		return fmt.Errorf("no model chosen for %s (pass --model)", n.name)
	}
	acct := ""
	if opts.Connect.Kept != nil {
		acct = opts.Connect.Kept(prov)
	}
	fresh := acct == ""
	if !fresh && !opts.Yes {
		use, err := p.Confirm(fmt.Sprintf("Use the %s account connected on this computer (%s)?", n.name, acct), true)
		if err != nil {
			return err
		}
		fresh = !use
	}
	if fresh {
		p.Say(fmt.Sprintf("%s: %s makes an API key for Agentry in your browser, billed to your %s account; it is kept in this computer's keychain, never in the policy file, for the runs on this computer (a run In Agentry spends Agentry's own model access).", ConnectLabel, n.name, n.name))
		var err error
		if acct, err = opts.Connect.Connect(prov); err != nil {
			return err
		}
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		def := ""
		if same {
			def = pol.Model.Name
		}
		if opts.Yes {
			model = def
		} else {
			ans, err := p.Ask(fmt.Sprintf("Model (the %s model id, such as %s; Agentry does not list them, and a model without tool calling fails)", n.name, n.example), def)
			if err != nil {
				return err
			}
			model = strings.TrimSpace(ans)
		}
	}
	if model == "" {
		return fmt.Errorf("no model chosen for %s (pass --model)", n.name)
	}
	keyEnv := strings.TrimSpace(opts.APIKeyEnv)
	if keyEnv == "" {
		keyEnv = d.APIKeyEnv
		if same && pol.Model.APIKeyEnv != "" {
			keyEnv = pol.Model.APIKeyEnv
		}
	}
	pol.Model = policy.Model{
		Provider:     prov,
		Name:         model,
		APIKeyEnv:    keyEnv,
		APIKeySource: modelkey.SourceKeyring,
		ProxyAllow:   append([]string(nil), d.ProxyAllow...),
	}
	who := ""
	if acct != "" {
		who = " as " + acct
	}
	p.Say(fmt.Sprintf("model: %s/%s (paid by your %s account, connected%s; key from $%s when it is set, else the connected key in the OS keychain; agent-container egress limited to %s)",
		prov, model, n.name, who, keyEnv, strings.Join(pol.Model.ProxyAllow, ", ")))
	return nil
}

// CopilotSignInLabel is the Copilot choice's wording.
const CopilotSignInLabel = "Sign in with GitHub"

// configureCopilot is the model setup for a Copilot plan: the sign-in, the
// model, the plan whose Copilot API host the proxy allows, and the policy's
// model block. No key variable and no base URL: Copilot CLI finds GitHub's
// endpoints itself, and the proxy allows GitHub's API and the plan's
// Copilot host alone.
func configureCopilot(p Prompter, pol *policy.Policy, opts ModelOpts) error {
	if opts.Copilot.SignIn == nil {
		return errors.New("this setup cannot sign in with GitHub")
	}
	// A plan named on the command line is held to the list before anything
	// is signed in for it.
	if plan := strings.ToLower(strings.TrimSpace(opts.CopilotPlan)); plan != "" {
		if _, err := policy.CopilotProxyAllow(plan); err != nil {
			return err
		}
	}
	acct := ""
	if opts.Copilot.Kept != nil {
		acct = opts.Copilot.Kept()
	}
	fresh := acct == ""
	if !fresh && !opts.Yes {
		use, err := p.Confirm(fmt.Sprintf("Use the GitHub sign-in kept on this computer (%s)?", acct), true)
		if err != nil {
			return err
		}
		fresh = !use
	}
	if fresh {
		p.Say(CopilotSignInLabel + ": your Copilot plan pays for the runs on this computer, through GitHub's Copilot CLI; hosted runs never use it.")
		var err error
		if acct, err = opts.Copilot.SignIn(); err != nil {
			return err
		}
	}
	samePlan := pol.Model.APIKeyKind == policy.KeyKindCopilotSignIn
	// The plan first: it names the Copilot API host the model list, as a
	// run, reaches.
	plan := strings.ToLower(strings.TrimSpace(opts.CopilotPlan))
	if plan == "" {
		def := policy.CopilotPlanIndividual
		if samePlan {
			if kept := policy.CopilotPlanOf(pol.Model.ProxyAllow); kept != "" {
				def = kept
			}
		}
		if opts.Yes {
			plan = def
		} else {
			ans, err := p.Ask("Your Copilot plan (individual = Free, Pro, Pro+ or Max | business | enterprise)", def)
			if err != nil {
				return err
			}
			plan = strings.ToLower(strings.TrimSpace(ans))
		}
	}
	allow, err := policy.CopilotProxyAllow(plan)
	if err != nil {
		return err
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		def := CopilotAuto
		if samePlan && pol.Model.Name != "" {
			def = pol.Model.Name
		}
		if opts.Yes {
			model = def
		} else {
			offered := copilotOffered(p, opts, plan)
			q := "Model (auto lets Copilot choose, the only choice on Copilot Free; or a Copilot model id such as claude-sonnet-5.5)"
			if offered != nil {
				q = "Model (your plan offers " + strings.Join(offered, ", ") + "; auto lets Copilot choose)"
				if !slices.Contains(offered, def) {
					def = CopilotAuto
				}
			}
			for {
				ans, err := p.Ask(q, def)
				if err != nil {
					return err
				}
				model = strings.TrimSpace(ans)
				if offered == nil || model == "" || slices.Contains(offered, model) {
					break
				}
				// A model the plan does not offer, a disabled one among them,
				// is not taken.
				p.Say(fmt.Sprintf("your Copilot plan does not offer %q: choose one of %s", model, strings.Join(offered, ", ")))
			}
		}
	}
	if model == "" {
		return errors.New("no model chosen for the Copilot plan (pass --model)")
	}
	pol.Model = policy.Model{
		Provider:   policy.ProviderCopilot,
		Name:       model,
		APIKeyKind: policy.KeyKindCopilotSignIn,
		ProxyAllow: allow,
	}
	who := ""
	if acct != "" {
		who = " as " + acct
	}
	p.Say(fmt.Sprintf("model: copilot/%s, paid by your Copilot plan (signed in with GitHub%s; runs use Copilot CLI, and the agent container reaches %s alone)",
		model, who, strings.Join(pol.Model.ProxyAllow, ", ")))
	return nil
}

// CopilotAuto is the Copilot model that lets Copilot choose, the only one
// a Copilot Free plan has; it is always offered.
const CopilotAuto = "auto"

// copilotOffered is what setup offers for plan: auto, then the models
// Copilot CLI lists, each once; nil when it lists none, or could not,
// which is said and the model typed instead.
func copilotOffered(p Prompter, opts ModelOpts, plan string) []string {
	if opts.Copilot.Models == nil {
		return nil
	}
	p.Say("Asking Copilot CLI which models your Copilot plan offers…")
	ids, err := opts.Copilot.Models(plan)
	if err != nil {
		p.Say(fmt.Sprintf("warning: could not list your plan's models (%v); type a model id instead", err))
		return nil
	}
	if len(ids) == 0 {
		return nil
	}
	out := []string{CopilotAuto}
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// configureClaude is the model setup for a Claude subscription through
// Claude Code: Claude Code's own sign-in, a model, and the policy's model
// block. No key variable and no base URL: Claude Code talks to Anthropic
// itself, and the proxy allows Anthropic's API and the host Claude Code
// refreshes its sign-in at, alone.
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
// allows it alone.
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
		// The one endpoint the compiler holds a plan run to.
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

	if prov == policy.ProviderAgentry {
		return configureAgentry(p, pol, opts)
	}

	d := DefaultsFor(prov)
	if d == nil && prov != "custom" && prov != policy.ProviderCopilot {
		// An unlisted provider name is passed to the harness as it is:
		// docker-agent knows more providers than the table, which only carries
		// the ones whose endpoints we can derive — but it does not know
		// "custom", which the adapter renders as its openai client at the base
		// URL, and it will not know a name made up here.
		p.Say(fmt.Sprintf("provider %q is not in the built-in table; the harness must know it by that name — for an OpenAI-compatible endpoint of your own, use custom with --base-url", prov))
	}

	// Copilot is paid by the person's Copilot plan alone.
	if prov == policy.ProviderCopilot {
		if kind := strings.ToLower(strings.TrimSpace(opts.APIKeyKind)); kind != "" && kind != policy.KeyKindCopilotSignIn {
			return fmt.Errorf("provider copilot is paid by your Copilot plan alone (api key kind %s), not %q", policy.KeyKindCopilotSignIn, kind)
		}
		return configureCopilot(p, pol, opts)
	} else if strings.EqualFold(strings.TrimSpace(opts.APIKeyKind), policy.KeyKindCopilotSignIn) {
		return fmt.Errorf("api key kind copilot-signin needs provider copilot (it is %q)", prov)
	}

	// OpenAI is paid by a key or by the person's ChatGPT plan:
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

	// Anthropic is paid by a key, a pasted subscription token, or
	// the person's Claude subscription through Claude Code's own sign-in:
	// the last asked when setup can start it.
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
			case "claude", policy.KeyKindClaudeSignIn:
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

	// OpenRouter, Poe and Hugging Face are paid by a key, pasted or minted
	// by connecting the person's account: asked when setup can
	// connect, unless the caller already said.
	if IsConnectProvider(prov) {
		connect := opts.ConnectAccount
		kind := strings.ToLower(strings.TrimSpace(opts.APIKeyKind))
		if connect && kind != "" && kind != policy.KeyKindAPI {
			return fmt.Errorf("%s pays with an API key (api key kind api), not %q", ConnectLabel, kind)
		}
		if !connect && kind == "" && !opts.Yes && opts.Connect.Connect != nil {
			def := "connect"
			if pol.Model.Provider == prov && (opts.Connect.Kept == nil || opts.Connect.Kept(prov) == "") {
				def = "key"
			}
			n := connectNames[prov].name
			ans, err := p.Ask(fmt.Sprintf("Pay for %s models with (connect = %s: %s makes a key for Agentry in your browser | key = a %s API key you paste)", n, ConnectLabel, n, n), def)
			if err != nil {
				return err
			}
			switch strings.ToLower(strings.TrimSpace(ans)) {
			case "connect":
				connect = true
			case "key", "api", "":
			default:
				return fmt.Errorf("%q is neither connect nor key", ans)
			}
		}
		if connect {
			return configureConnect(p, pol, opts, prov)
		}
	} else if opts.ConnectAccount {
		return fmt.Errorf("%s is for openrouter, poe and huggingface (the provider is %q)", ConnectLabel, prov)
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
			// The same-provider rule covers the URL too: staying on custom
			// keeps the endpoint the policy has — the question's default, and
			// under --yes the answer — and derives the allowlist from it again.
			// A switch brings no URL along.
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
		// Plain http off this machine is allowed and said out loud:
		// the key rides every model call.
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
					// The pasted key replaced a connected one: the connect
					// is forgotten, so no run renews or refuses this key.
					if IsConnectProvider(prov) && opts.Connect.Forget != nil {
						if out, err := opts.Connect.Forget(prov); err != nil {
							p.Say(fmt.Sprintf("warning: the %s account connected earlier is still marked connected (%v); remove it (agentry model-signins remove %s)", connectNames[prov].name, err, prov))
						} else if out != "" {
							p.Say(fmt.Sprintf("the key you pasted replaces the one your %s account connected: %s", connectNames[prov].name, out))
						}
					}
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
		// The shared sentence stands alone; a prefix made it ungrammatical.
		p.Say(policy.SubscriptionTokenSentence)
	}
	return nil
}

// hostPortOf derives the proxy allowlist entry from a base URL — the
// agent container's ONLY egress, so it must be exact.
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

// GuidedProfileOpts parameterizes the guided profile flow.
type GuidedProfileOpts struct {
	Ref    string
	Digest string
	// CapabilityLines is the contract display (what the keys are FOR).
	CapabilityLines []string
	// Secrets are the manifest-declared env names to collect.
	Secrets []string
	// Optional are the ones a run may go without (the manifest's
	// optionalSecrets): asked the same way, and said so.
	Optional []string
	// DefaultKeep seeds the keep question (policy default or flag).
	DefaultKeep bool
	// KeepDecided suppresses the keep question (the flag already chose).
	KeepDecided bool
	// Help is the release guide's connect help per secret name:
	// where to create the key, which scopes it needs, in markdown that may
	// span lines. Printed as text before the question for that
	// key, beneath its name behind termtext's gutter when it spans lines.
	Help map[string]string
}

// GuidedProfile runs the guided conversation and returns the new profile.
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
			// The help is markdown that may span lines, so it is
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

// configureAgentry records provider agentry: the model is paid with the
// credits of the account signed in to opts.AgentryRegistry, and reached at
// that registry's model endpoint. No key and no model is asked for: each
// run opens a credits session with the sign-in, and the registry forwards
// to the model its staff chose.
func configureAgentry(p Prompter, pol *policy.Policy, opts ModelOpts) error {
	if kind := strings.TrimSpace(opts.APIKeyKind); kind != "" {
		return fmt.Errorf("provider agentry is paid with your Agentry credits and takes no api key kind (got %q)", kind)
	}
	if opts.APIKeyEnv != "" || opts.APIKeySource != "" || opts.BaseURL != "" {
		return errors.New("provider agentry is paid with your Agentry credits through the registry you are signed in to: it takes no key variable, key source or base URL")
	}
	reg := strings.TrimRight(strings.TrimSpace(opts.AgentryRegistry), "/")
	if reg == "" {
		return errors.New("provider agentry pays with the credits of a SecureAgentry account: sign in first (agentry login), then run setup again")
	}
	m := policy.Model{Provider: policy.ProviderAgentry, Name: AgentryModelName, BaseURL: reg + policy.AgentryInferencePath}
	if _, err := policy.AgentryRegistry(m); err != nil {
		return err
	}
	hp, err := hostPortOf(reg)
	if err != nil {
		return err
	}
	m.ProxyAllow = []string{hp}
	if opts.Model != "" && opts.Model != AgentryModelName {
		p.Say(fmt.Sprintf("provider agentry uses the model %s's staff chose for runs paid with credits; --model %s is not used", reg, opts.Model))
	}
	pol.Model = m
	p.Say(fmt.Sprintf("model: paid with your Agentry credits at %s (each run opens a credits session with your sign-in; the agent container reaches %s alone and never holds your sign-in)", reg, hp))
	return nil
}

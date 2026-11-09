package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/claudesignin"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/savedmodels"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

func newProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List, add and remove the providers your saved models are paid with",
		Long: "A provider is a credential: a vendor's API key, a model sign-in (your ChatGPT, Claude or Copilot plan),\n" +
			"a connected account (OpenRouter, Poe, Hugging Face), your Agentry credits, or an endpoint of your own\n" +
			"(custom). A named vendor has one provider; each endpoint of your own is a provider of its own, named\n" +
			"custom-<name>. A second saved model on a provider asks for no new key or sign-in. A key is never\n" +
			"written to the policy: the provider names its variable, and the OS keychain entry provider:<id>.\n" +
			"list shows each kept sign-in and connected account beside the provider it pays, and the ones no\n" +
			"provider uses; add on a provider paid by a sign-in or connected account signs in or connects again;\n" +
			"remove of a provider that is not here removes a sign-in or connected account still kept for it.",
	}
	cmd.AddCommand(newProvidersListCmd(), newProvidersAddCmd(), newProvidersRemoveCmd())
	return cmd
}

// providerOut is one provider in --json: names and states, never a
// credential.
type providerOut struct {
	ID           string   `json:"id"`
	Vendor       string   `json:"vendor"`
	Name         string   `json:"name,omitempty"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"baseUrl,omitempty"`
	APIKeyEnv    string   `json:"apiKeyEnv,omitempty"`
	APIKeySource string   `json:"apiKeySource,omitempty"`
	State        string   `json:"state"`
	Models       []string `json:"models"`
	// The sign-in or connected account that pays the provider, as
	// model-signins list shows it: never a token.
	Account      string `json:"account,omitempty"`
	CreatedAt    string `json:"createdAt,omitempty"`
	LastUsedAt   string `json:"lastUsedAt,omitempty"`
	App          string `json:"app,omitempty"`
	KeyExpiresAt string `json:"keyExpiresAt,omitempty"`
	// SignedInAgain marks an add that signed in or connected again on a
	// provider already here, which it left as it was.
	SignedInAgain bool `json:"signedInAgain,omitempty"`
}

// keptSignInOut is one kept sign-in or connected account in providers list
// --json: model-signins list's row, with the provider it pays for and
// whether a provider here is paid by it.
type keptSignInOut struct {
	modelSignInOut
	Provider string `json:"provider"`
	InUse    bool   `json:"inUse"`
}

// providerRow is a provider as listed: its credential's state, the saved
// models on it, and the kept sign-in or connected account that pays it
// (rec, nil for none).
func providerRow(home, id string, pr policy.Provider, models []string, rec *modelsignin.Record) providerOut {
	kind, state := providerState(home, id, pr)
	r := providerOut{ID: id, Vendor: policy.ProviderVendor(id), Name: pr.Name, Kind: kind, BaseURL: pr.BaseURL,
		APIKeyEnv: pr.APIKeyEnv, APIKeySource: pr.APIKeySource, State: state, Models: models}
	if r.Models == nil {
		r.Models = []string{}
	}
	if rec != nil {
		r.Account, r.CreatedAt, r.LastUsedAt, r.App, r.KeyExpiresAt = rec.Label, rec.CreatedAt, rec.LastUsedAt, rec.App, rec.KeyExpiresAt
	}
	return r
}

// providerLine is a provider row in words.
func providerLine(r providerOut) string {
	name := ""
	if r.Name != "" {
		name = " (" + r.Name + ", " + r.BaseURL + ")"
	}
	details := ""
	if r.CreatedAt != "" {
		if r.Account != "" && !strings.Contains(r.State, r.Account) {
			details += " · as " + r.Account
		}
		if r.App != "" {
			details += " · through Agentry's " + mcpauth.GitHubAppName(r.App)
		}
		if r.KeyExpiresAt != "" {
			details += " · key expires " + r.KeyExpiresAt
		}
		last := r.LastUsedAt
		if last == "" {
			last = "never used"
		}
		details += " · last used: " + last
	}
	return fmt.Sprintf("%s%s · %s · %s%s · models: %s", r.ID, name, r.Kind, r.State, details, strings.Join(r.Models, ", "))
}

// savedModelsOn names the saved models on provider id.
func savedModelsOn(models []policy.SavedModel, id string) []string {
	on := []string{}
	for _, sm := range models {
		if sm.Provider == id {
			on = append(on, sm.Name)
		}
	}
	return on
}

// removeKeptCommand is the command that removes a kept sign-in or connected
// account: the provider's own removal when no provider of its id is here,
// else the removal of the sign-in alone.
func removeKeptCommand(k savedmodels.KeptSignIn) string {
	c := "agentry providers remove " + k.Provider
	if k.ProviderHere {
		c += " " + signInOnlyFlag(k.Record.Vendor)
	}
	return c
}

// signInOnlyFlag is the providers remove flag that removes vendor's kept
// credential alone: --connect for a connected account, --sign-in otherwise.
func signInOnlyFlag(vendor string) string {
	if modelsignin.IsConnectVendor(vendor) {
		return "--connect"
	}
	return "--sign-in"
}

// signInVendor is the model sign-in store's vendor a provider's credential
// is kept under, "" for a key or credits.
func signInVendor(id string, pr policy.Provider) string { return savedmodels.SignInVendor(id, pr) }

// providerState says whether the provider's credential is here.
func providerState(home, id string, pr policy.Provider) (kind, state string) {
	s := savedmodels.ProviderState(home, id, pr, credentialFor)
	return s.Kind, s.Words
}

func newProvidersListCmd() *cobra.Command {
	var policyPath string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the providers, with the saved models on each and whether its credential is here (never a key)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pol, _, err := modelsPolicy(policyPath)
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			providers, models, _ := pol.SavedModels()
			kept, err := savedmodels.SignIns(home, providers)
			if err != nil {
				return err
			}
			paying := map[string]*modelsignin.Record{}
			signIns := make([]keptSignInOut, 0, len(kept))
			var unused []savedmodels.KeptSignIn
			for _, k := range kept {
				if k.InUse {
					paying[k.Provider] = &k.Record
				} else {
					unused = append(unused, k)
				}
				signIns = append(signIns, keptSignInOut{modelSignInOut: modelSignInRow(k.Record), Provider: k.Provider, InUse: k.InUse})
			}
			ids := make([]string, 0, len(providers))
			for id := range providers {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			rows := make([]providerOut, 0, len(ids))
			for _, id := range ids {
				rows = append(rows, providerRow(home, id, providers[id], savedModelsOn(models, id), paying[id]))
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"providers": rows, "signIns": signIns})
			}
			out := cmd.OutOrStdout()
			for _, r := range rows {
				_, _ = fmt.Fprintln(out, providerLine(r))
			}
			if len(unused) > 0 {
				_, _ = fmt.Fprintln(out, "\nKept sign-ins and connected accounts no provider uses:")
				for _, k := range unused {
					_, _ = fmt.Fprintf(out, "  %s · remove: %s\n", signInLine(modelSignInRow(k.Record)), removeKeptCommand(k))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

// providerFlags are `providers add`'s.
type providerFlags struct {
	name, baseURL, keyEnv, keySource, copilotPlan, policyPath  string
	keyStdin, signIn, connect, installed, oauthApp, newAccount bool
}

// providerShapingFlags are the add flags that say what a provider is. On a
// provider already here, add signs in again and changes none of it.
var providerShapingFlags = []string{"name", "base-url", "api-key-env", "api-key-source", "key-stdin", "copilot-plan"}

// signInFlagsRefusal refuses a sign-in flag on a vendor it is not for.
func signInFlagsRefusal(vendor string, f providerFlags) error {
	usage := func(msg string) error { return engine.Classify(engine.ExitUsage, errors.New(msg)) }
	switch {
	case f.newAccount && (vendor != policy.ProviderOpenAI || !f.signIn):
		return usage("--new-account is for openai --sign-in: it signs in to another ChatGPT account than the one this computer is registered for")
	case f.installed && (vendor != policy.ProviderAnthropic || !f.signIn):
		return usage("--installed is for anthropic --sign-in")
	case f.oauthApp && vendor != policy.ProviderCopilot:
		return usage("--oauth-app is for copilot")
	}
	return nil
}

// signInAgainVendor is the kept sign-in or connected account that add signs
// in or connects again on pr, a provider already here: the one that pays it,
// when the request names that way of paying; "" when add is refused.
// reconnect says a connect may write the provider's keychain entry: it holds
// a connected account's key, or none. A key the person pasted there is
// never replaced by a connect.
func signInAgainVendor(vendor string, f providerFlags, pr policy.Provider, reconnect bool) string {
	switch {
	case vendor == policy.ProviderOpenAI && f.signIn && pr.APIKeyKind == policy.KeyKindChatGPTPlan:
		return modelsignin.ChatGPT
	case vendor == policy.ProviderAnthropic && f.signIn && pr.APIKeyKind == policy.KeyKindClaudeSignIn:
		return modelsignin.Claude
	case vendor == policy.ProviderCopilot && pr.APIKeyKind == policy.KeyKindCopilotSignIn:
		return modelsignin.Copilot
	case f.connect && modelsignin.IsConnectVendor(vendor) && pr.APIKeySource == modelkey.SourceKeyring && reconnect:
		return vendor
	}
	return ""
}

// mayReconnect says a connect may write provider id's keychain entry: a
// connected account is kept for it, or the entry holds no key.
func mayReconnect(home, id string) bool {
	if r, err := modelsignin.Open(home, nil).Get(modelsignin.ConnectedVendor(id)); err == nil && r.Connected {
		return true
	}
	present, err := connectKeyPresent(id)
	return err == nil && !present
}

// signInAgain signs in with vendor, or connects its account, again for the
// provider id already here, replacing the kept credential; the provider and
// its saved models are left as they are.
func signInAgain(cmd *cobra.Command, home, id string, pr policy.Provider, vendor string, models []policy.SavedModel, f providerFlags) error {
	for _, name := range providerShapingFlags {
		if cmd.Flags().Changed(name) {
			return engine.Classify(engine.ExitUsage, fmt.Errorf("--%s would change the provider %s, which is here already: add on it signs in or connects again and changes nothing else (agentry providers remove %s first to set it up again)", name, id, id))
		}
	}
	in := bufio.NewReader(cmd.InOrStdin())
	var err error
	switch vendor {
	case modelsignin.ChatGPT:
		_, err = signInChatGPT(cmd, in, home, f.newAccount)
		var ne *mcpauth.ChatGPTNotEligibleError
		if errors.As(err, &ne) || errors.Is(err, mcpauth.ErrChatGPTNoPlanScope) {
			err = fmt.Errorf("nothing changed: %w", err)
		}
	case modelsignin.Claude:
		_, err = signInClaude(cmd, in, home, f.installed)
	case modelsignin.Copilot:
		_, err = signInCopilot(cmd, home, copilotApp(f.oauthApp))
	default:
		if _, err = connectAccount(cmd, in, home, vendor); err == nil {
			// The key in the provider's entry changed: a signed-out
			// schedule's sealed copy follows it.
			resealSignedOut(cmd.ErrOrStderr())
		}
	}
	if err != nil {
		return err
	}
	rec, _ := modelsignin.Open(home, nil).Get(vendor)
	on := savedModelsOn(models, id)
	row := providerRow(home, id, pr, on, rec)
	row.SignedInAgain = true
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), row)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "signed in again for the provider %s · %s · %s\n", id, row.Kind, row.State)
	if len(on) == 0 {
		_, _ = fmt.Fprintln(out, "the provider is as it was; it has no saved models")
	} else {
		_, _ = fmt.Fprintf(out, "the provider and its saved models (%s) are as they were\n", strings.Join(on, ", "))
	}
	return nil
}

func newProvidersAddCmd() *cobra.Command {
	var f providerFlags
	cmd := &cobra.Command{
		Use:   "add <vendor>",
		Short: "Add a provider: a vendor's key, your plan's sign-in, a connected account, Agentry credits, or an endpoint of your own (custom)",
		Long: "add <vendor> takes a key: anthropic, openai, google, vultr, openrouter, poe, huggingface. The key stays\n" +
			"in its variable (--api-key-env), or with --key-stdin it is read from the standard input and kept in\n" +
			"the OS keychain as provider:<vendor>. --sign-in pays with your plan instead: openai (your ChatGPT\n" +
			"plan), anthropic (your Claude subscription, through Claude Code); copilot is always your Copilot\n" +
			"plan. --connect connects your OpenRouter, Poe or Hugging Face account. agentry pays with your\n" +
			"Agentry credits, through the registry you are signed in to.\n\n" +
			"add custom --name <name> --base-url <url> adds an OpenAI-compatible endpoint of your own, as the\n" +
			"provider custom-<name>. Without --api-key-env or --key-stdin it is saved as taking no key, and a run on\n" +
			"it is given none; with one, a run on it is refused while its key is not here. Then save a model on\n" +
			"the provider: agentry models add.\n\n" +
			"On a provider already here that your plan's sign-in or a connected account pays, add signs in or\n" +
			"connects again (openai --sign-in [--new-account], anthropic --sign-in [--installed], copilot,\n" +
			"openrouter|poe|huggingface --connect) and replaces the kept credential; the provider and its saved\n" +
			"models are left as they are. A provider paid by a key is set up again by removing it first.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pol, path, err := modelsPolicy(f.policyPath)
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			vendor := strings.ToLower(strings.TrimSpace(args[0]))
			if err := signInFlagsRefusal(vendor, f); err != nil {
				return err
			}
			// One provider per vendor, one per endpoint of your own. On a
			// provider here that a sign-in or connected account pays, add
			// signs in or connects again; any other id already here is
			// refused before any sign-in starts or any key is read, so
			// nothing kept is replaced.
			if id := providerIDFor(vendor, f.name); id != "" {
				providers, models, _ := pol.SavedModels()
				if pr, ok := providers[id]; ok {
					if sv := signInAgainVendor(vendor, f, pr, mayReconnect(home, id)); sv != "" {
						return signInAgain(cmd, home, id, pr, sv, models, f)
					}
					return engine.Classify(engine.ExitUsage, fmt.Errorf("this computer already has the provider %s: one provider per vendor (agentry providers remove %s first to set it up again)", id, id))
				}
			}
			id, pr, key, err := buildProvider(cmd, home, vendor, f)
			if err != nil {
				return err
			}
			if err := pol.AddProvider(id, pr); err != nil {
				return engine.Classify(engine.ExitUsage, err)
			}
			if key != "" {
				if err := modelkey.Store(id, key); err != nil {
					return engine.Classify(engine.ExitPolicy, fmt.Errorf("keep the key in the OS keychain: %w", err))
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "kept the key in the OS keychain as %s\n", modelkey.Account(id))
			}
			if err := policy.Save(path, pol); err != nil {
				if key != "" {
					_ = modelkey.Delete(id)
				}
				return engine.Classify(engine.ExitPolicy, err)
			}
			var rec *modelsignin.Record
			if sv := signInVendor(id, pr); sv != "" {
				rec, _ = modelsignin.Open(home, nil).Get(sv)
			}
			row := providerRow(home, id, pr, nil, rec)
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), row)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added the provider %s · %s · %s\n", id, row.Kind, row.State)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "save a model on it: agentry models add <name> --provider %s --model <model id>\n", id)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.name, "name", "", "custom: the endpoint's name, which names the provider custom-<name>")
	fl.StringVar(&f.baseURL, "base-url", "", "custom (or openai): the endpoint's base URL, e.g. http://127.0.0.1:11434/v1")
	fl.StringVar(&f.keyEnv, "api-key-env", "", "the variable NAME holding the key (default: the vendor's usual one)")
	fl.StringVar(&f.keySource, "api-key-source", "", "where the key comes from when the variable is unset: env | keyring")
	fl.BoolVar(&f.keyStdin, "key-stdin", false, "read the key from the standard input and keep it in the OS keychain (never in the policy)")
	fl.BoolVar(&f.signIn, "sign-in", false, "openai or anthropic: pay with your ChatGPT plan or Claude subscription instead of a key")
	fl.BoolVar(&f.newAccount, "new-account", false, "openai --sign-in: sign in to another ChatGPT account than the one this computer is registered for")
	fl.BoolVar(&f.installed, "installed", false, "anthropic --sign-in: sign in the Claude Code installed on this computer rather than its image")
	fl.BoolVar(&f.connect, "connect", false, "openrouter, poe or huggingface: "+setup.ConnectLabel+" instead of a key you paste")
	fl.StringVar(&f.copilotPlan, "copilot-plan", "", "copilot: individual (the default) | business | enterprise")
	fl.BoolVar(&f.oauthApp, "oauth-app", false, "copilot: sign in through Agentry's OAuth App rather than its GitHub App")
	fl.StringVar(&f.policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

// providerIDFor is the provider id vendor and a custom endpoint's name make,
// "" when they make none.
func providerIDFor(vendor, name string) string {
	if vendor != policy.ProviderCustom {
		return vendor
	}
	id, err := policy.CustomProviderID(name)
	if err != nil {
		return ""
	}
	return id
}

// buildProvider makes the provider vendor names from the flags, signing in
// or connecting first where it pays that way and nothing is kept yet. A key
// read with --key-stdin is handed back, for the caller to keep once the
// provider is added.
func buildProvider(cmd *cobra.Command, home, vendor string, f providerFlags) (id string, pr policy.Provider, key string, err error) {
	usage := func(format string, a ...any) error {
		return engine.Classify(engine.ExitUsage, fmt.Errorf(format, a...))
	}
	in := bufio.NewReader(cmd.InOrStdin())
	if f.name != "" && vendor != policy.ProviderCustom {
		return "", pr, "", usage("--name is for custom, an endpoint of your own")
	}
	switch {
	case vendor == policy.ProviderAgentry:
		reg := agentryRegistryFor(vendor)
		if reg == "" {
			return "", pr, "", usage("provider agentry pays with the credits of a SecureAgentry account: sign in first (agentry login)")
		}
		hp, err := setup.EndpointHostPort(reg)
		if err != nil {
			return "", pr, "", err
		}
		return vendor, policy.Provider{BaseURL: strings.TrimRight(reg, "/") + policy.AgentryInferencePath, ProxyAllow: []string{hp}}, "", nil
	case vendor == policy.ProviderCopilot:
		plan := f.copilotPlan
		if plan == "" {
			plan = policy.CopilotPlanIndividual
		}
		allow, err := policy.CopilotProxyAllow(plan)
		if err != nil {
			return "", pr, "", usage("--copilot-plan: %v", err)
		}
		if kept, _ := modelsignin.Open(home, nil).Get(modelsignin.Copilot); kept == nil || kept.Broken != "" {
			if _, err := signInCopilot(cmd, home, copilotApp(f.oauthApp)); err != nil {
				return "", pr, "", err
			}
		}
		return vendor, policy.Provider{APIKeyKind: policy.KeyKindCopilotSignIn, ProxyAllow: allow}, "", nil
	case f.signIn:
		id, pr, err := signInProvider(cmd, in, home, vendor, f)
		return id, pr, "", err
	case vendor == policy.ProviderCustom:
		if strings.TrimSpace(f.name) == "" || strings.TrimSpace(f.baseURL) == "" {
			return "", pr, "", usage("custom needs --name and --base-url: an endpoint of your own is a provider of its own, named for it")
		}
		id, err := policy.CustomProviderID(f.name)
		if err != nil {
			return "", pr, "", usage("%v", err)
		}
		// With no key flag the endpoint is saved as taking no key, and a
		// run on it is given none. A key kept in the keychain is read
		// after a variable of the provider's own name.
		def := ""
		if f.keyStdin || strings.TrimSpace(f.keySource) == modelkey.SourceKeyring {
			def = savedmodels.CustomKeyEnv(id)
		}
		pr, key, err := keyProvider(cmd, in, id, def, nil, f)
		pr.Name = strings.TrimSpace(f.name)
		return id, pr, key, err
	}
	d := setup.DefaultsFor(vendor)
	if d == nil && f.baseURL == "" {
		return "", pr, "", usage("this build has no endpoint for %q: for an OpenAI-compatible endpoint of your own, add custom --name <name> --base-url <url>", vendor)
	}
	if f.connect {
		if !modelsignin.IsConnectVendor(vendor) {
			return "", pr, "", usage("--connect is for openrouter, poe and huggingface")
		}
		if kept, _ := modelsignin.Open(home, nil).Get(vendor); kept == nil || !kept.Connected || kept.Broken != "" {
			if _, err := connectAccount(cmd, in, home, vendor); err != nil {
				return "", pr, "", err
			}
		}
		return vendor, policy.Provider{APIKeyEnv: d.APIKeyEnv, APIKeySource: modelkey.SourceKeyring, ProxyAllow: append([]string(nil), d.ProxyAllow...)}, "", nil
	}
	def, allow := strings.ToUpper(vendor)+"_API_KEY", []string(nil)
	if d != nil {
		def, allow = d.APIKeyEnv, append([]string(nil), d.ProxyAllow...)
	}
	pr, key, err = keyProvider(cmd, in, vendor, def, allow, f)
	return vendor, pr, key, err
}

// keyProvider is a provider paid by a key: its variable, and the endpoint.
// With --key-stdin the key is read from the standard input and handed back
// to be kept in the keychain, never written to the policy.
func keyProvider(cmd *cobra.Command, in *bufio.Reader, id, defEnv string, allow []string, f providerFlags) (policy.Provider, string, error) {
	pr := policy.Provider{APIKeyEnv: f.keyEnv, APIKeySource: f.keySource, BaseURL: strings.TrimSpace(f.baseURL), ProxyAllow: allow}
	if pr.APIKeyEnv == "" {
		pr.APIKeyEnv = defEnv
	}
	if pr.APIKeySource == modelkey.SourceEnv {
		pr.APIKeySource = ""
	}
	if pr.APIKeySource != "" && pr.APIKeySource != modelkey.SourceKeyring {
		return pr, "", engine.Classify(engine.ExitUsage, fmt.Errorf("--api-key-source %q must be env or keyring", f.keySource))
	}
	if pr.BaseURL != "" {
		hp, err := setup.EndpointHostPort(pr.BaseURL)
		if err != nil {
			return pr, "", engine.Classify(engine.ExitUsage, err)
		}
		pr.ProxyAllow = []string{hp}
		if w := setup.PlainHTTPWarning(pr.BaseURL); w != "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w)
		}
	}
	if !f.keyStdin {
		if os.Getenv(pr.APIKeyEnv) == "" && pr.APIKeySource != modelkey.SourceKeyring && policy.ProviderVendor(id) != policy.ProviderCustom {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: $%s is not set here: export it where agentry runs, or add the provider with --key-stdin to keep the key in the OS keychain\n", pr.APIKeyEnv)
		}
		return pr, "", nil
	}
	if modelkey.Disabled() {
		return pr, "", engine.Classify(engine.ExitUsage, fmt.Errorf("--key-stdin keeps the key in the OS keychain, which is disabled here (%s is set): set $%s instead", modelkey.EnvNoKeyring, pr.APIKeyEnv))
	}
	line, err := in.ReadString('\n')
	key := strings.TrimSpace(line)
	if key == "" {
		if err != nil && !errors.Is(err, io.EOF) {
			return pr, "", err
		}
		return pr, "", engine.Classify(engine.ExitUsage, errors.New("--key-stdin: the standard input held no key"))
	}
	pr.APIKeySource = modelkey.SourceKeyring
	if k := policy.CredentialKindOf(policy.ProviderVendor(id), key); k != policy.KeyKindAPI {
		pr.APIKeyKind = k
	}
	return pr, key, nil
}

// signInProvider is a provider paid by the person's plan: the vendor's
// sign-in, made now when none is kept.
func signInProvider(cmd *cobra.Command, in *bufio.Reader, home, vendor string, f providerFlags) (string, policy.Provider, error) {
	store := modelsignin.Open(home, nil)
	switch vendor {
	case policy.ProviderOpenAI:
		if kept, _ := store.Get(modelsignin.ChatGPT); kept == nil || kept.Broken != "" || f.newAccount {
			if _, err := signInChatGPT(cmd, in, home, f.newAccount); err != nil {
				return "", policy.Provider{}, err
			}
		}
		return vendor, policy.Provider{APIKeyKind: policy.KeyKindChatGPTPlan, ProxyAllow: policy.SignInProxyAllow(policy.KeyKindChatGPTPlan)}, nil
	case policy.ProviderAnthropic:
		kept, _ := store.Get(modelsignin.Claude)
		if kept == nil || (f.installed && kept.HostConfigDir == "") || (!f.installed && claudesignin.Kept(home) == "" && kept.HostConfigDir == "") {
			if _, err := signInClaude(cmd, in, home, f.installed); err != nil {
				return "", policy.Provider{}, err
			}
		}
		return vendor, policy.Provider{APIKeyKind: policy.KeyKindClaudeSignIn, ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn)}, nil
	}
	return "", policy.Provider{}, engine.Classify(engine.ExitUsage, fmt.Errorf("--sign-in is for openai (your ChatGPT plan) and anthropic (your Claude subscription); copilot always signs in"))
}

func newProvidersRemoveCmd() *cobra.Command {
	var policyPath, moveTo string
	var keep, signInOnly, connectOnly bool
	cmd := &cobra.Command{
		Use:   "remove <id> [--move-to <saved model>]",
		Short: "Remove a provider and the saved models on it, and forget its credential on this computer",
		Long: "The saved models on the provider are removed with it. While a schedule or a persistent agent\n" +
			"names one of them the removal is refused, with them listed, unless --move-to names the saved model\n" +
			"they move to; it is also the new default when the default is on this provider. The provider's key is\n" +
			"deleted from the OS keychain and its sign-in or connected account removed (revoked at the vendor\n" +
			"where this build can; for anthropic, Claude Code logs out and its directory is deleted; a connected\n" +
			"key is forgotten, with where to delete it at the vendor), unless --keep-credential.\n\n" +
			"When no provider <id> is here but a sign-in or connected account is still kept for it (openai: your\n" +
			"ChatGPT sign-in; anthropic: Claude; copilot; openrouter, poe, huggingface), remove removes that.\n" +
			"--sign-in (openai, anthropic, copilot) or --connect (openrouter, poe, huggingface) removes the kept\n" +
			"sign-in or connected account alone and leaves the provider and its saved models.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pol, path, err := modelsPolicy(policyPath)
			if err != nil {
				return err
			}
			home, err := runnerHome()
			if err != nil {
				return err
			}
			id := args[0]
			providers, models, _ := pol.SavedModels()
			pr, ok := providers[id]
			if !ok || signInOnly || connectOnly {
				return removeKeptSignIn(cmd, pol, home, id, providers, keptOnly{signIn: signInOnly, connect: connectOnly, moveTo: moveTo, keep: keep})
			}
			var on []policy.SavedModel
			for _, sm := range models {
				if sm.Provider == id {
					on = append(on, sm)
				}
			}
			out, err := removeModels(cmd, pol, path, fmt.Sprintf("the provider %s has saved models", id), on, moveTo, func(newDefault string) error {
				_, err := pol.RemoveProvider(id, newDefault)
				return err
			})
			if err != nil {
				return err
			}
			out.Provider, out.ProviderRemoved, out.Credential = id, true, "credential kept"
			if out.Removed == nil {
				out.Removed = []savedModelOut{}
			}
			if !keep {
				out.Credential, out.SignIn, out.Revocation = forgetCredential(cmd, home, id, pr)
			}
			if flags.jsonOut {
				if err := emitJSON(cmd.OutOrStdout(), out); err != nil {
					return err
				}
				sayCopilotApps(cmd.ErrOrStderr(), out.SignIn, out.Revocation)
				return nil
			}
			sayRemoved(cmd.OutOrStdout(), pol, out)
			sayCopilotApps(cmd.OutOrStdout(), out.SignIn, out.Revocation)
			return nil
		},
	}
	cmd.Flags().StringVar(&moveTo, "move-to", "", "the saved model what names this provider's models moves to, and the new default when the default is on it")
	cmd.Flags().BoolVar(&keep, "keep-credential", false, "keep the provider's key, sign-in or connected account on this computer")
	cmd.Flags().BoolVar(&signInOnly, "sign-in", false, "openai, anthropic or copilot: remove the kept sign-in alone and leave the provider")
	cmd.Flags().BoolVar(&connectOnly, "connect", false, "openrouter, poe or huggingface: disconnect the kept account alone and leave the provider")
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

// keptOnly is a providers remove of a kept sign-in or connected account,
// with the flags it was given.
type keptOnly struct {
	signIn, connect, keep bool
	moveTo                string
}

// removeKeptSignIn removes the sign-in or connected account kept for the
// provider id: one no provider uses, or, with --sign-in or --connect, the
// one kept beside a provider here, which is left as it is.
func removeKeptSignIn(cmd *cobra.Command, pol *policy.Policy, home, id string, providers map[string]policy.Provider, k keptOnly) error {
	usage := func(format string, a ...any) error {
		return engine.Classify(engine.ExitUsage, fmt.Errorf(format, a...))
	}
	vendor := modelsignin.VendorOfProvider(id)
	pr, here := providers[id]
	switch {
	case k.signIn && k.connect:
		return usage("--sign-in and --connect: one or the other")
	case k.signIn && (vendor == "" || modelsignin.IsConnectVendor(vendor)):
		return usage("--sign-in is for openai, anthropic and copilot, whose sign-ins are kept on this computer")
	case k.connect && !modelsignin.IsConnectVendor(vendor):
		return usage("--connect is for openrouter, poe and huggingface, whose connected accounts are kept on this computer")
	case vendor == "" && modelsignin.ProviderOfVendor(id) != "":
		p := modelsignin.ProviderOfVendor(id)
		return usage("no provider %q on this computer: %s is kept as the provider %s's credential (agentry providers remove %s %s)", id, id, p, p, signInOnlyFlag(id))
	case vendor == "":
		return usage("no provider %q on this computer (agentry providers list)", id)
	case k.moveTo != "" || k.keep:
		return usage("--move-to and --keep-credential are for removing a provider: this removes only the sign-in or connected account kept for %s", id)
	}
	outcome, err := removeSignIn(cmd, home, vendor)
	switch {
	case errors.Is(err, modelsignin.ErrNone) && here:
		return usage("no %s sign-in or connected account is kept on this computer", vendor)
	case errors.Is(err, modelsignin.ErrNone):
		return usage("no provider %q on this computer, and no sign-in or connected account kept for it (agentry providers list)", id)
	case err != nil:
		return err
	}
	out := removedOut{Provider: id, SignIn: vendor, Revocation: outcome, Credential: removedWords(vendor, outcome),
		Removed: []savedModelOut{}, Moved: []modelUser{}, Default: pol.DefaultModel().ID}
	if flags.jsonOut {
		if err := emitJSON(cmd.OutOrStdout(), out); err != nil {
			return err
		}
		sayCopilotApps(cmd.ErrOrStderr(), vendor, outcome)
		return nil
	}
	w := cmd.OutOrStdout()
	sayRemovedSignIn(w, vendor, outcome)
	if here {
		next := ""
		if signInVendor(id, pr) == vendor {
			again := "a sign-in"
			if modelsignin.IsConnectVendor(vendor) {
				again = "connecting"
			}
			next = "; it needs " + again + " again before a run can use it (" + modelsignin.SignInCommand(vendor) + ")"
		}
		_, _ = fmt.Fprintf(w, "the provider %s is kept%s\n", id, next)
	}
	return nil
}

// removeSignIn removes vendor's kept sign-in or connected account as the
// person asked: revoked at the vendor where this build can, Claude Code
// logged out and its directory deleted, a connected key forgotten; the
// keys of schedules that run signed out are sealed again. ErrNone when
// none is kept.
func removeSignIn(cmd *cobra.Command, home, vendor string) (string, error) {
	s := connectStore(home)
	// A Claude sign-in is Claude Code's, in its own directories: Claude
	// Code logs out there, then they are deleted.
	s.EndClaude = endClaude(cmd.ErrOrStderr(), home)
	outcome, err := s.Remove(cmd.Context(), vendor, "removed by the person")
	if err != nil {
		return "", err
	}
	resealSignedOut(cmd.ErrOrStderr())
	return outcome, nil
}

// removedWords is a removed sign-in or connected account, in the words
// providers remove's credential field uses.
func removedWords(vendor, outcome string) string {
	if modelsignin.IsConnectVendor(vendor) {
		return "connected account removed, revocation: " + outcome
	}
	return "sign-in removed, revocation: " + outcome
}

// sayRemovedSignIn says a sign-in or connected account was removed.
func sayRemovedSignIn(w io.Writer, vendor, outcome string) {
	if modelsignin.IsConnectVendor(vendor) {
		_, _ = fmt.Fprintf(w, "disconnected your %s account (revocation: %s)\n", modelsignin.ConnectName(vendor), outcome)
	} else {
		_, _ = fmt.Fprintf(w, "removed the %s sign-in (revocation: %s)\n", vendor, outcome)
	}
	sayCopilotApps(w, vendor, outcome)
}

// sayCopilotApps says what GitHub keeps after a Copilot sign-in's removal:
// GitHub takes the tokens for revocation, and the app's authorization, which
// only a holder of its client secret could revoke, stays.
func sayCopilotApps(w io.Writer, vendor, outcome string) {
	if vendor == modelsignin.Copilot && outcome == modelsignin.RevokeSubmitted {
		_, _ = fmt.Fprintln(w, "GitHub accepted the sign-in's tokens for revocation; Agentry's app stays in your authorized apps until you remove it there: https://github.com/settings/applications")
	}
}

// forgetCredential forgets a removed provider's credential on this
// computer, and says what became of it, with the sign-in or connected
// account removed and its revocation, if any.
func forgetCredential(cmd *cobra.Command, home, id string, pr policy.Provider) (credential, signIn, revocation string) {
	if sv := signInVendor(id, pr); sv != "" {
		outcome, err := removeSignIn(cmd, home, sv)
		switch {
		case errors.Is(err, modelsignin.ErrNone):
		case err != nil:
			return "its sign-in is still kept: " + err.Error() + " (agentry providers remove " + id + ")", "", ""
		default:
			return removedWords(sv, outcome), sv, outcome
		}
	}
	if pr.APIKeySource == modelkey.SourceKeyring {
		if ok, _ := modelkey.Present(id); ok {
			if err := modelkey.Delete(id); err != nil {
				return "its key is still in the OS keychain: " + err.Error(), "", ""
			}
			resealSignedOut(cmd.ErrOrStderr())
			return "key deleted from the OS keychain", "", ""
		}
	}
	return "nothing kept on this computer", "", ""
}

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

	"github.com/blaktron/agentry-cli/pkg/runner/claudesignin"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
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
			"agentry model-signins is the sign-in half of this, kept under its own name for now.",
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
}

// signInVendor is the model sign-in store's vendor a provider's credential
// is kept under, "" for a key or credits.
func signInVendor(id string, pr policy.Provider) string {
	switch pr.APIKeyKind {
	case policy.KeyKindChatGPTPlan:
		return modelsignin.ChatGPT
	case policy.KeyKindClaudeSignIn:
		return modelsignin.Claude
	case policy.KeyKindCopilotSignIn:
		return modelsignin.Copilot
	}
	if modelsignin.IsConnectVendor(id) {
		return id
	}
	return ""
}

// providerState says whether the provider's credential is here.
func providerState(home, id string, pr policy.Provider) (kind, state string) {
	vendor := policy.ProviderVendor(id)
	if vendor == policy.ProviderAgentry {
		reg, _ := policy.AgentryRegistry(policy.Model{BaseURL: pr.BaseURL})
		if tok, err := credentialFor(reg); err != nil || tok == "" {
			return "Agentry credits", "needs sign-in (agentry login)"
		}
		return "Agentry credits", "signed in at " + reg
	}
	if sv := signInVendor(id, pr); sv != "" && pr.APIKeyKind != policy.KeyKindAPI && pr.APIKeyKind != "" {
		r, err := modelsignin.Open(home, nil).Get(sv)
		switch {
		case err != nil:
			return policy.SignInSentence(pr.APIKeyKind), "needs sign-in"
		case r.Broken != "":
			return policy.SignInSentence(pr.APIKeyKind), "needs sign-in: " + r.Broken
		case r.Label != "":
			return policy.SignInSentence(pr.APIKeyKind), "signed in as " + r.Label
		}
		return policy.SignInSentence(pr.APIKeyKind), "signed in"
	}
	if pr.APIKeyEnv == "" && vendor == policy.ProviderCustom {
		return "takes no key", "ready"
	}
	kind = "key from $" + pr.APIKeyEnv
	if pr.APIKeySource == modelkey.SourceKeyring {
		kind += ", else the OS keychain entry " + modelkey.Account(id)
	}
	if r, err := modelsignin.Open(home, nil).Get(id); err == nil && modelsignin.IsConnectVendor(id) && r.Connected {
		kind = "connected account"
	}
	if os.Getenv(pr.APIKeyEnv) != "" {
		return kind, "key set"
	}
	if pr.APIKeySource == modelkey.SourceKeyring {
		if ok, err := modelkey.Present(id); err == nil && ok {
			return kind, "key in the keychain"
		}
	}
	return kind, "key missing"
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
			ids := make([]string, 0, len(providers))
			for id := range providers {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			rows := make([]providerOut, 0, len(ids))
			for _, id := range ids {
				pr := providers[id]
				kind, state := providerState(home, id, pr)
				on := []string{}
				for _, sm := range models {
					if sm.Provider == id {
						on = append(on, sm.Name)
					}
				}
				rows = append(rows, providerOut{ID: id, Vendor: policy.ProviderVendor(id), Name: pr.Name, Kind: kind, BaseURL: pr.BaseURL,
					APIKeyEnv: pr.APIKeyEnv, APIKeySource: pr.APIKeySource, State: state, Models: on})
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), map[string]any{"providers": rows})
			}
			for _, r := range rows {
				name := ""
				if r.Name != "" {
					name = " (" + r.Name + ", " + r.BaseURL + ")"
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s%s · %s · %s · models: %s\n", r.ID, name, r.Kind, r.State, strings.Join(r.Models, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

// providerFlags are `providers add`'s.
type providerFlags struct {
	name, baseURL, keyEnv, keySource, copilotPlan, policyPath string
	keyStdin, signIn, connect, installed, oauthApp            bool
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
			"the provider: agentry models add.",
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
			// One provider per vendor, one per endpoint of your own: an id
			// already here is refused before any sign-in starts or any key
			// is read, so nothing kept is replaced.
			if id := providerIDFor(vendor, f.name); id != "" {
				if providers, _, _ := pol.SavedModels(); hasProvider(providers, id) {
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
			kind, state := providerState(home, id, pr)
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), providerOut{ID: id, Vendor: policy.ProviderVendor(id), Name: pr.Name, Kind: kind, BaseURL: pr.BaseURL,
					APIKeyEnv: pr.APIKeyEnv, APIKeySource: pr.APIKeySource, State: state, Models: []string{}})
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added the provider %s · %s · %s\n", id, kind, state)
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

func hasProvider(providers map[string]policy.Provider, id string) bool {
	_, ok := providers[id]
	return ok
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
			if err := addCopilot(cmd, home, f.oauthApp); err != nil {
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
			def = strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_API_KEY"
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
		if kept, _ := store.Get(modelsignin.ChatGPT); kept == nil || kept.Broken != "" {
			if _, err := signInChatGPT(cmd, in, home, false); err != nil {
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
	var keep bool
	cmd := &cobra.Command{
		Use:   "remove <id> [--move-to <saved model>]",
		Short: "Remove a provider and the saved models on it, and forget its credential on this computer",
		Long: "The saved models on the provider are removed with it. While a schedule or a persistent agent\n" +
			"names one of them the removal is refused, with them listed, unless --move-to names the saved model\n" +
			"they move to; it is also the new default when the default is on this provider. The provider's key is\n" +
			"deleted from the OS keychain and its sign-in or connected account removed as agentry model-signins\n" +
			"remove does, unless --keep-credential.",
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
			if !ok {
				return engine.Classify(engine.ExitUsage, fmt.Errorf("no provider %q on this computer (agentry providers list)", id))
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
			out.Provider, out.Credential = id, "credential kept"
			if out.Removed == nil {
				out.Removed = []savedModelOut{}
			}
			if !keep {
				out.Credential = forgetCredential(cmd, home, id, pr)
			}
			if flags.jsonOut {
				return emitJSON(cmd.OutOrStdout(), out)
			}
			sayRemoved(cmd.OutOrStdout(), pol, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&moveTo, "move-to", "", "the saved model what names this provider's models moves to, and the new default when the default is on it")
	cmd.Flags().BoolVar(&keep, "keep-credential", false, "keep the provider's key, sign-in or connected account on this computer")
	cmd.Flags().StringVar(&policyPath, "policy", "", "operator policy file (default ~/.agentry/runner/policy.yaml)")
	return cmd
}

// forgetCredential forgets a removed provider's credential on this
// computer, and says what became of it.
func forgetCredential(cmd *cobra.Command, home, id string, pr policy.Provider) string {
	if sv := signInVendor(id, pr); sv != "" {
		s := connectStore(home)
		s.EndClaude = endClaude(cmd.ErrOrStderr(), home)
		outcome, err := s.Remove(cmd.Context(), sv, "removed by the person")
		switch {
		case errors.Is(err, modelsignin.ErrNone):
		case err != nil:
			return "its sign-in is still kept: " + err.Error() + " (agentry model-signins remove " + sv + ")"
		default:
			resealSignedOut(cmd.ErrOrStderr())
			return "sign-in removed, revocation: " + outcome
		}
	}
	if pr.APIKeySource == modelkey.SourceKeyring {
		if ok, _ := modelkey.Present(id); ok {
			if err := modelkey.Delete(id); err != nil {
				return "its key is still in the OS keychain: " + err.Error()
			}
			resealSignedOut(cmd.ErrOrStderr())
			return "key deleted from the OS keychain"
		}
	}
	return "nothing kept on this computer"
}

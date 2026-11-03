package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// runhostModel is the model a runhost's own runs use, its persistent
// agents': an API provider and model, and the key, kept in its store.
type runhostModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"apiKey,omitempty"`
}

// runhostPutModel answers the store verb's put-model: the key goes into the
// runhost's store, and the runhost's policy names the provider and model,
// the key's source its store.
func runhostPutModel(in io.Reader, fw *runhost.FrameWriter) (int, error) {
	var m runhostModel
	dec := json.NewDecoder(io.LimitReader(in, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return engine.ExitUsage, fmt.Errorf("the model: %w", err)
	}
	m.Provider = strings.ToLower(strings.TrimSpace(m.Provider))
	switch {
	case setup.DefaultsFor(m.Provider) == nil || !providerRE.MatchString(m.Provider):
		return engine.ExitUsage, fmt.Errorf("%q is not a provider a runhost keeps a key for", m.Provider)
	case m.Model == "" || strings.ContainsAny(m.Model, "\n\x00"):
		return engine.ExitUsage, errors.New("the model names no model")
	case m.APIKey == "" || strings.ContainsAny(m.APIKey, "\n\r\x00 "):
		return engine.ExitUsage, errors.New("the model's key is not one line")
	}
	path, err := policy.DefaultPath()
	if err != nil {
		return engine.ExitCLI, err
	}
	pol, _, err := policy.EnsureDefault(path)
	if err != nil {
		return engine.ExitPolicy, err
	}
	if err := modelkey.Store(m.Provider, m.APIKey); err != nil {
		return engine.ExitCLI, fmt.Errorf("keep the key in the runhost's store: %w", err)
	}
	opts := setup.ModelOpts{Provider: m.Provider, Model: m.Model, APIKeySource: modelkey.SourceKeyring, APIKeyKind: "api", Yes: true}
	if err := setup.ConfigureModel(refusingPrompter{}, pol, opts); err != nil {
		return engine.ExitPolicy, err
	}
	if err := pol.Validate(); err != nil {
		return engine.ExitPolicy, err
	}
	if err := policy.Save(path, pol); err != nil {
		return engine.ExitCLI, err
	}
	return 0, fw.Result(runhostModel{Provider: pol.Model.Provider, Model: pol.Model.Name})
}

// refusingPrompter answers no question: the runhost's setup asks nobody.
type refusingPrompter struct{}

func (refusingPrompter) Ask(string, string) (string, error) {
	return "", errors.New("the runhost asks no question")
}
func (refusingPrompter) AskSecret(string) (string, error) {
	return "", errors.New("the runhost asks no question")
}
func (refusingPrompter) Confirm(string, bool) (bool, error) {
	return false, errors.New("the runhost asks no question")
}
func (refusingPrompter) Say(string) {}

// runRunhostModelKey gives the runhost name an API provider's model and key
// for its own runs: its persistent agents'. The key is read here, from
// keyEnv or this computer's keychain, and kept on the runhost, encrypted,
// after the person agrees.
func runRunhostModelKey(cmd *cobra.Command, name, provider, model, keyEnv string, yes bool) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	d := setup.DefaultsFor(provider)
	if d == nil {
		return engine.Classify(engine.ExitUsage, fmt.Errorf("--provider %q is not a provider (%s)", provider, strings.Join(setup.Providers(), ", ")))
	}
	if model == "" {
		model = d.Model
	}
	if keyEnv == "" {
		keyEnv = d.APIKeyEnv
	}
	key := strings.TrimSpace(os.Getenv(keyEnv))
	if key == "" {
		if k, err := modelkey.Lookup(provider); err == nil {
			key = strings.TrimSpace(k)
		}
	}
	if key == "" {
		return engine.Classify(engine.ExitPolicy, fmt.Errorf("no %s key here: set $%s, or store it with agentry setup", provider, keyEnv))
	}
	if !yes {
		ask := terminalAsker(cmd)
		if ask == nil {
			return engine.Classify(engine.ExitUsage, errors.New("no one is at a terminal to confirm: pass --yes"))
		}
		ok, err := ask.Confirm(fmt.Sprintf("Keep your %s API key on runhost %s, encrypted in its store, for the persistent agents that run there on %s?", provider, name, model), false)
		if err != nil {
			return err
		}
		if !ok {
			return engine.Classify(engine.ExitPolicy, errors.New("nothing was kept"))
		}
	}
	body, err := json.Marshal(runhostModel{Provider: provider, Model: model, APIKey: key})
	if err != nil {
		return err
	}
	a, _, err := askRunhostInput(cmd, name, "store", []string{"put-model"}, body)
	if err != nil {
		return engine.Classify(engine.ExitGateway, err)
	}
	if a.Code != 0 {
		return engine.Classify(a.Code, fmt.Errorf("runhost %s: %s", name, a.Error))
	}
	var got runhostModel
	_ = json.Unmarshal(a.Result, &got)
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), map[string]any{"runhost": name, "model": got})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "runhost %s keeps your %s key; its persistent agents run on %s\n", name, got.Provider, got.Model)
	return nil
}

// askRunhostInput is askRunhostArgs with body as the verb's input.
func askRunhostInput(cmd *cobra.Command, name, verb string, args []string, body []byte) (runhost.Answer, *runhost.Record, error) {
	c, rec, err := dialAgentRunhost(cmd, name)
	if err != nil {
		return runhost.Answer{}, rec, err
	}
	defer func() { _ = c.Close() }()
	a, err := c.Call(cmd.Context(), verb, args, func(w io.Writer) error {
		_, err := w.Write(body)
		return err
	}, func(f runhost.Frame) error {
		if f.Type == runhost.FrameOut {
			_, _ = fmt.Fprintln(narration(cmd), f.Text)
		}
		return nil
	})
	return a, rec, err
}

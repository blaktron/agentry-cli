// Connect your account (plan model-sign-ins.md §4.6, D30–D34;
// agentry-cli#742): `agentry model-signins add openrouter|poe|huggingface`
// and the offer in `agentry setup`. The flows are mcpauth's OpenRouter, Poe
// and HuggingFace; the store is modelsignin.KeepConnected, which keeps the
// key in the model key's keychain entry; this is the terminal around them.
// The key is never printed, logged or written outside the keychain (I6).
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// The connect flows' seams: nil endpoints are the vendors' own; the model
// key's keychain calls are modelkey's. A test points both at fakes.
var (
	openRouterEndpoints  func(*mcpauth.OpenRouter)
	poeEndpoints         func(*mcpauth.Poe)
	huggingFaceEndpoints func(*mcpauth.HuggingFace)
	connectStoreKey      = modelkey.Store
	connectDeleteKey     = modelkey.Delete
)

// connectStore is the model sign-in store with the model key's keychain
// entry, where a connected key is kept.
func connectStore(home string) *modelsignin.Store {
	s := modelsignin.Open(home, connectionKeys())
	s.StoreKey, s.DeleteKey = connectStoreKey, connectDeleteKey
	return s
}

// connectAccount runs vendor's connect in the browser, with the address
// shown and, where the browser is on another machine, the redirect pasted
// back, and keeps the key it minted.
func connectAccount(cmd *cobra.Command, in *bufio.Reader, home, vendor string) (*modelsignin.Record, error) {
	name := modelsignin.ConnectName(vendor)
	if name == "" {
		return nil, fmt.Errorf("%s is not an account this build connects (openrouter, poe, huggingface)", vendor)
	}
	if modelkey.Disabled() {
		return nil, fmt.Errorf("%s keeps the key in the OS keychain, which is disabled here (%s is set): paste a key into the variable instead (agentry setup)", setup.ConnectLabel, modelkey.EnvNoKeyring)
	}
	errOut := cmd.ErrOrStderr()
	pasted := make(chan string)
	done := make(chan struct{})
	var finished chan struct{}
	kc := mcpauth.KeyConnect{
		HTTP:   vendorHTTP(),
		Pasted: pasted,
		Say:    func(s string) { _, _ = fmt.Fprintln(errOut, s) },
		Present: func(authURL string) {
			_, _ = fmt.Fprintf(errOut, "Connect your %s account in your browser:\n  %s\n", name, authURL)
			if os.Getenv("AGENTRY_NO_BROWSER") == "" {
				if err := openBrowser(authURL); err != nil {
					_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v; open the address above yourself)\n", err)
				}
			}
			_, _ = fmt.Fprintln(errOut, "Waiting for the browser (up to 10 minutes). If it is on another machine and ends on a page that does not load, paste that page's address here and press Enter:")
			finished = make(chan struct{})
			go readPasted(in, pasted, done, finished)
		},
	}
	got, err := runConnect(cmd.Context(), kc, vendor)
	close(done)
	if finished != nil {
		select {
		case <-finished:
		default:
			_, _ = fmt.Fprintln(errOut, "Press Enter to continue.")
			<-finished
		}
	}
	if err != nil {
		return nil, err
	}
	return connectStore(home).KeepConnected(vendor, got)
}

// runConnect is vendor's flow, as the client this build presents.
func runConnect(ctx context.Context, kc mcpauth.KeyConnect, vendor string) (*mcpauth.ConnectedKey, error) {
	switch vendor {
	case modelsignin.OpenRouter:
		f := mcpauth.OpenRouter{KeyConnect: kc}
		if openRouterEndpoints != nil {
			openRouterEndpoints(&f)
		}
		return f.Connect(ctx)
	case modelsignin.Poe:
		id, err := mcpauth.PoeClientID()
		if err != nil {
			return nil, err
		}
		f := mcpauth.Poe{KeyConnect: kc}
		if poeEndpoints != nil {
			poeEndpoints(&f)
		}
		return f.Connect(ctx, id)
	case modelsignin.HuggingFace:
		f := mcpauth.HuggingFace{KeyConnect: kc}
		if huggingFaceEndpoints != nil {
			huggingFaceEndpoints(&f)
		}
		return f.Connect(ctx, mcpauth.HuggingFaceClientID())
	}
	return nil, fmt.Errorf("%s is not an account this build connects", vendor)
}

// connectOffer is what `agentry setup` offers of a connect.
func connectOffer(cmd *cobra.Command, in *bufio.Reader, home string) setup.ConnectOffer {
	return setup.ConnectOffer{
		Kept: func(provider string) string {
			r, err := modelsignin.Open(home, nil).Get(modelsignin.ConnectedVendor(provider))
			if err != nil || r.Broken != "" {
				return ""
			}
			if r.Label == "" {
				return "an account " + modelsignin.ConnectName(r.Vendor) + " did not name"
			}
			return r.Label
		},
		Connect: func(provider string) (string, error) {
			r, err := connectAccount(cmd, in, home, modelsignin.ConnectedVendor(provider))
			if err != nil {
				return "", err
			}
			return r.Label, nil
		},
	}
}

// addConnect is `agentry model-signins add openrouter|poe|huggingface`.
func addConnect(cmd *cobra.Command, in *bufio.Reader, home, vendor string) error {
	r, err := connectAccount(cmd, in, home, vendor)
	if err != nil {
		return err
	}
	if flags.jsonOut {
		return emitJSON(cmd.OutOrStdout(), signedInJSON{Vendor: vendor, Kind: r.Kind, Connected: true, Account: r.Label, KeyExpiresAt: r.KeyExpiresAt, Models: []string{}})
	}
	name, who := modelsignin.ConnectName(vendor), ""
	if r.Label != "" {
		who = " as " + r.Label
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "connected your %s account%s; its key is in the OS keychain as %s\n", name, who, modelkey.Account(r.Provider))
	if r.KeyExpiresAt != "" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "the key expires at %s, as you chose at %s; connect again then\n", r.KeyExpiresAt, name)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "to run on it: agentry setup (provider %s, pay with connect); your %s account then pays for the runs on this computer\n", r.Provider, name)
	return nil
}

// refreshConnectedKey readies a connected key for a run that reads one from
// the keychain, at its start (modelsignin.Store.ReadyForRun): Hugging
// Face's token is refreshed into the entry (D32), a Poe key past its expiry
// refused. A hosted run never reads this computer's keys.
func refreshConnectedKey(ctx context.Context, home string, plan *spec.Plan, hosted bool) error {
	if hosted {
		return nil
	}
	if err := connectStore(home).ReadyForRun(ctx, plan); err != nil {
		return engine.Classify(engine.ExitPolicy, err)
	}
	return nil
}

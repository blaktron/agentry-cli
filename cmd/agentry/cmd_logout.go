package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the selected registry's local login (does not revoke its token)",
		Args:  cobra.NoArgs,
		RunE:  runLogout,
	}
}

func runLogout(cmd *cobra.Command, _ []string) error {
	reg, err := authRegistry()
	if err != nil {
		if flags.jsonOut {
			return authFailure(cmd, "", "failed", "invalid_registry")
		}
		return fmt.Errorf("sign out: invalid registry")
	}
	fail := func(reason string) error {
		if flags.jsonOut {
			return authFailure(cmd, reg, "failed", reason)
		}
		return fmt.Errorf("sign out: %s", reason)
	}
	cfg, err := loadCLIConfig()
	if err != nil {
		return fail("storage_unavailable")
	}
	entry := cfg.findRegistry(reg)
	if entry != nil && entry.TokenRef == tokenRefKeyring {
		host, err := registryHost(reg)
		if err != nil {
			return fail("invalid_registry")
		}
		// Existing host-keyed credentials can alias multiple URL identities
		// (#267). Refuse rather than deleting another registry's credential.
		for _, other := range cfg.Registries {
			if strings.TrimRight(other.URL, "/") == reg || other.TokenRef == tokenRefFile || other.TokenRef == tokenRefNone {
				continue
			}
			otherHost, err := registryHost(other.URL)
			if err == nil && otherHost == host {
				return fail("shared_keyring_identity")
			}
		}
		if err := keyringDelete(host); err != nil {
			return fail("keyring_unavailable")
		}
	}
	// Preserve registry choice, trust pins, install consent and unrelated
	// identities. The tombstone prevents a stale keyring fallback signing
	// this URL back in after its file-backed token was removed.
	cfg.upsertRegistry(registryEntry{URL: reg, TokenRef: tokenRefNone})
	if err := saveCLIConfig(cfg); err != nil {
		return fail("storage_unavailable")
	}
	// The token a signed-out schedule sealed goes with the sign-in.
	resealSignedOut(cmd.ErrOrStderr())
	override := strings.TrimSpace(os.Getenv(envToken)) != ""
	if flags.jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(authEvent{Format: 1, Status: "signed_out", Registry: reg, EnvironmentCredential: override})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed local login for %s (the registry token was not revoked)\n", reg)
	if override {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "AGENTRY_TOKEN is still set and overrides local sign-out; unset it separately")
	}
	return nil
}

// `agentry login` (§11): read a PAT from stdin, verify it against GET /user,
// store it — OS keychain first, config-file fallback for headless machines —
// and pin the registry's signing keys (TOFU).
//
// §11's login row also names a "device-code or browser flow". That needs
// server-side device-authorization endpoints which do not exist; the PAT
// paste remains the only way in, recorded in DEVIATIONS as DEV-025 rather
// than left as a silent gap.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// newLoginCmd builds the login command.
func newLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate to a registry with a personal access token",
		Long: "Read a personal access token (agy_…) from stdin, verify it against the\n" +
			"registry, and store it: in the OS keychain when available, else in\n" +
			"~/.config/agentry/config.yaml (chmod 600) with a warning.\n\n" +
			"Login also PINS the registry's DSSE signing keys on first use (§8.8\n" +
			"trust-on-first-use). Pinning here rather than at the first verify is\n" +
			"deliberate: login is the moment someone deliberately chose this registry\n" +
			"and typed a credential for it, which is when a trust-on-first-use decision\n" +
			"belongs — not at whichever download happens to come first. A registry that\n" +
			"advertises key ids differing from the pinned set is reported, never\n" +
			"silently re-pinned.\n\n" +
			"Scripts may instead set " + envToken + " to bypass stored credentials.",
		Args: cobra.NoArgs,
		RunE: runLogin,
	}
}

// runLogin executes the login flow.
func runLogin(cmd *cobra.Command, _ []string) error {
	reg := registryURL()

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Paste a personal access token for %s (needs the user:profile scope): ", reg)
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("read token from stdin: %w", err)
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return errors.New("no token provided")
	}

	client, err := newAPIClientChecked(cmd, reg, token)
	if err != nil {
		return err
	}
	resp, err := client.GetCurrentUserWithResponse(cmd.Context())
	if err != nil {
		return fmt.Errorf("verify token against %s: %w", reg, err)
	}
	if resp.JSON200 == nil {
		return apiProblem("verify token", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	handle := resp.JSON200.Handle

	host, err := registryHost(reg)
	if err != nil {
		return err
	}
	entry := registryEntry{URL: reg, TokenRef: tokenRefKeyring}
	storage := "OS keychain"
	if kerr := keyringSet(host, token); kerr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v — storing the token in the config file instead (chmod 600)\n", kerr)
		entry.TokenRef = tokenRefFile
		entry.Token = token
		storage = "config file"
	}

	cfg, err := loadCLIConfig()
	if err != nil {
		return err
	}
	cfg.upsertRegistry(entry)
	if cfg.DefaultRegistry == "" {
		cfg.DefaultRegistry = reg
	}
	if err := saveCLIConfig(cfg); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "logged in to %s as %s (token stored in %s)\n", reg, handle, storage)

	// §11: "pins signing keys (TOFU)". Never fatal — an unsigned self-host
	// registry is a legitimate configuration, and a key-fetch hiccup must not
	// undo a login whose credential is already stored and verified.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "signing keys: %s\n", pinSigningKeysAtLogin(cmd.Context(), reg))
	return nil
}

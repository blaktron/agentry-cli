// `agentry login` (§11): obtain a PAT — through the browser (the default on
// a terminal: the registry's authorize page mints a one-time code to a
// loopback listener and this process exchanges it, agentry-cli#5 /
// DEV-025 closure) or pasted from stdin (`--paste`, and the default when
// stdin is not a terminal) — verify it against GET /user, store it (OS
// keychain first, config-file fallback for headless machines), and pin the
// registry's signing keys (TOFU).
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/blaktron/agentry-cli/pkg/authflow"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// openBrowser hands a URL to the system browser; tests replace it.
var openBrowser = func(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	return c.Start()
}

// stdinIsTerminal reports whether the command's input is an interactive
// terminal — the case where a browser sign-in is the better default. An
// injected reader (tests, a pipe) is never a terminal.
func stdinIsTerminal(in io.Reader) bool { return stdinIsTTY(in) }

// newLoginCmd builds the login command.
func newLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate to a registry with a personal access token",
		Long: "Sign in through the browser (default on a terminal): the registry's\n" +
			"authorize page hands this process a one-time code on 127.0.0.1 and the\n" +
			"token is minted on exchange, never shown. Or paste a personal access\n" +
			"token (agy_…) on stdin with --paste (the default when stdin is not a\n" +
			"terminal). Either way the token is verified against the registry and\n" +
			"stored: in the OS keychain when available, else in\n" +
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
	cmd.Flags().Bool("browser", false, "sign in through the browser even when stdin is not a terminal")
	cmd.Flags().Bool("paste", false, "paste a personal access token on stdin instead of signing in through the browser")
	cmd.Flags().Duration("timeout", authflow.Timeout, "browser sign-in deadline (positive, at most 5 minutes; JSON mode only)")
	cmd.Long += "\n\nWith --json, sign-in always uses the browser and writes a pending NDJSON\n" +
		"event followed by a typed final result. No token is accepted on stdin.\n" +
		"The host may send {\"action\":\"cancel\"} or {\"action\":\"decline\"} on stdin;\n" +
		"SIGINT/SIGTERM also cancel. AGENTRY_TOKEN must be unset for this flow."
	return cmd
}

// runLogin executes the login flow.
func runLogin(cmd *cobra.Command, _ []string) error {
	if flags.jsonOut {
		return runMachineLogin(cmd)
	}
	reg := registryURL()
	forceBrowser, _ := cmd.Flags().GetBool("browser")
	forcePaste, _ := cmd.Flags().GetBool("paste")
	if forceBrowser && forcePaste {
		return errors.New("--browser and --paste are mutually exclusive")
	}
	useBrowser := forceBrowser || (!forcePaste && stdinIsTerminal(cmd.InOrStdin()))

	var token string
	if useBrowser {
		t, err := loginViaBrowser(cmd, reg)
		if err != nil {
			return err
		}
		token = t
	} else {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Paste a personal access token for %s (needs the user:profile scope): ", reg)
		reader := bufio.NewReader(cmd.InOrStdin())
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read token from stdin: %w", err)
		}
		token = strings.TrimSpace(line)
		if token == "" {
			return errors.New("no token provided")
		}
	}

	_, err := completeLogin(cmd, reg, token)
	return err
}

// completeLogin keeps browser and terminal clients on the same verification,
// storage, signing-key pinning and Knowledge rewrap path.
func completeLogin(cmd *cobra.Command, reg, token string) (authEvent, error) {
	result := authEvent{Format: 1, Status: "signed_in", Registry: reg}
	// A sign-in credential belongs to the selected registry. Never follow
	// a redirect while verifying it; /meta is public and needs no token.
	hc := *httpClient
	hc.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	client, err := newAPIClientWithHTTP(reg, token, &hc)
	if err != nil {
		return result, err
	}
	if err := minVersionGate(cmd.Context(), cmd.ErrOrStderr(), reg, ""); err != nil {
		return result, err
	}
	resp, err := client.GetCurrentUserWithResponse(cmd.Context())
	if err != nil {
		return result, fmt.Errorf("verify token against %s: %w", reg, err)
	}
	if resp.JSON200 == nil {
		return result, apiProblem("verify token", resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
	}
	handle := resp.JSON200.Handle
	if err := cmd.Context().Err(); err != nil {
		return result, err
	}
	cfg, err := loadCLIConfig()
	if err != nil {
		return result, err
	}

	host, err := registryHost(reg)
	if err != nil {
		return result, err
	}
	entry := registryEntry{URL: reg, TokenRef: tokenRefKeyring}
	storage := "OS keychain"
	if kerr := keyringSet(host, token); kerr != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v — storing the token in the config file instead (chmod 600)\n", kerr)
		entry.TokenRef = tokenRefFile
		entry.Token = token
		storage = "config file"
	}

	cfg.upsertRegistry(entry)
	if cfg.DefaultRegistry == "" {
		cfg.DefaultRegistry = reg
	}
	if err := saveCLIConfig(cfg); err != nil {
		return result, err
	}

	if !flags.jsonOut {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "logged in to %s as %s (token stored in %s)\n", reg, handle, storage)
	}

	// §11: "pins signing keys (TOFU)". Never fatal — an unsigned self-host
	// registry is a legitimate configuration, and a key-fetch hiccup must not
	// undo a login whose credential is already stored and verified.
	pins := trust.PinAtLoginResult(cmd.Context(), httpClient, reg)
	if !flags.jsonOut {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "signing keys: %s\n", pins.Note)
	}
	rewrapAfterLogin(cmd, reg)
	resealSignedOut(cmd.ErrOrStderr())
	result.Handle = strings.ReplaceAll(handle, token, "[redacted]")
	result.Storage = entry.TokenRef
	result.SigningKeys = pins.Status
	return result, nil
}

// loginViaBrowser runs the loopback flow: a one-shot listener, the
// registry's authorize page in the browser, a code back, the token on
// exchange. The URL is always printed so a machine without a browser
// association can be completed by hand. That browser must reach this
// process's loopback listener; AGENTRY_NO_BROWSER=1 skips the open attempt.
func loginViaBrowser(cmd *cobra.Command, reg string) (string, error) {
	flow, err := authflow.Start()
	if err != nil {
		return "", err
	}
	defer flow.Close()
	url := flow.AuthorizeURL(reg)
	errOut := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(errOut, "Sign in to %s in your browser:\n  %s\n", reg, url)
	if os.Getenv("AGENTRY_NO_BROWSER") == "" {
		if err := openBrowser(url); err != nil {
			_, _ = fmt.Fprintf(errOut, "(could not open a browser: %v — open the address above yourself)\n", err)
		}
	}
	_, _ = fmt.Fprintln(errOut, "Waiting for the browser to finish (up to 5 minutes; Ctrl-C to abort, or use --paste)…")
	return flow.Wait(cmd.Context())
}

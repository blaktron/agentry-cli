package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/authflow"
	"github.com/spf13/cobra"
)

// authEvent is the versioned NDJSON lifecycle, deliberately without raw
// transport, registry or storage error text: those may echo credentials.
type authEvent struct {
	Format                int    `json:"format"`
	Status                string `json:"status"`
	Registry              string `json:"registry,omitempty"`
	AuthorizeURL          string `json:"authorizeUrl,omitempty"`
	ExpiresInSeconds      int    `json:"expiresInSeconds,omitempty"`
	Handle                string `json:"handle,omitempty"`
	Storage               string `json:"storage,omitempty"`
	SigningKeys           string `json:"signingKeys,omitempty"`
	Reason                string `json:"reason,omitempty"`
	EnvironmentCredential bool   `json:"environmentCredential,omitempty"`
}

func authFailure(cmd *cobra.Command, reg, status, reason string) error {
	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(authEvent{Format: 1, Registry: reg, Status: status, Reason: reason}); err != nil {
		return errors.New("could not write authentication result")
	}
	return errors.New("authentication: " + reason)
}

// Resolve without registryURL's diagnostic path: malformed local state must
// not send its contents into a machine client's stderr/model context.
func authRegistry() (string, error) {
	reg := flags.registry
	if reg == "" {
		cfg, err := loadCLIConfig()
		if err != nil {
			return "", err
		}
		reg = cfg.DefaultRegistry
	}
	if reg == "" {
		reg = defaultRegistryURL
	}
	reg = strings.TrimRight(reg, "/")
	u, err := url.Parse(reg)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("invalid registry")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return "", errors.New("registry requires HTTPS (or loopback HTTP)")
	}
	return reg, nil
}

var errAuthDeclined = errors.New("declined")
var errAuthControl = errors.New("invalid_control")

func runMachineLogin(cmd *cobra.Command) error {
	reg, err := authRegistry()
	if err != nil {
		return authFailure(cmd, "", "failed", "invalid_registry")
	}
	paste, _ := cmd.Flags().GetBool("paste")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	if paste || timeout <= 0 || timeout > authflow.Timeout {
		return authFailure(cmd, reg, "failed", "invalid_options")
	}
	if strings.TrimSpace(os.Getenv(envToken)) != "" {
		return authFailure(cmd, reg, "failed", "environment_credential")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	ctx, controlCancel := context.WithCancelCause(ctx)
	defer controlCancel(nil)
	flow, err := authflow.Start()
	if err != nil {
		return authFailure(cmd, reg, "failed", "listener_unavailable")
	}
	defer flow.Close()
	authorize := flow.AuthorizeURL(reg)
	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(authEvent{Format: 1, Status: "pending", Registry: reg, AuthorizeURL: authorize, ExpiresInSeconds: int((timeout + time.Second - 1) / time.Second)}); err != nil {
		return errors.New("could not write authentication result")
	}
	// EOF alone is not cancellation: detached callers may use /dev/null.
	// The process owns this reader; no independent helper survives its exit.
	go readAuthControl(cmd.InOrStdin(), controlCancel)
	if os.Getenv("AGENTRY_NO_BROWSER") == "" {
		_ = openBrowser(authorize)
	}
	token, err := flow.Wait(ctx)
	if ctx.Err() != nil {
		return authContextFailure(cmd, reg, ctx)
	}
	if err != nil {
		if errors.Is(err, authflow.ErrDeclined) {
			return authFailure(cmd, reg, "declined", "declined")
		}
		if errors.Is(err, authflow.ErrTimeout) {
			return authFailure(cmd, reg, "timed_out", "timed_out")
		}
		return authFailure(cmd, reg, "failed", "browser_exchange_failed")
	}
	// Existing helpers may narrate remote problems or keyring errors.
	// Never forward raw text to a machine client. Report storage/pin status.
	previousErr := cmd.ErrOrStderr()
	cmd.SetErr(io.Discard)
	defer cmd.SetErr(previousErr)
	previousContext := cmd.Context()
	cmd.SetContext(ctx)
	defer cmd.SetContext(previousContext)
	result, err := completeLogin(cmd, reg, token)
	if err != nil {
		if ctx.Err() != nil {
			return authContextFailure(cmd, reg, ctx)
		}
		return authFailure(cmd, reg, "failed", "verification_or_storage_failed")
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
}

func authContextFailure(cmd *cobra.Command, reg string, ctx context.Context) error {
	status, reason := "cancelled", "cancelled"
	switch {
	case errors.Is(context.Cause(ctx), errAuthDeclined):
		status, reason = "declined", "declined"
	case errors.Is(context.Cause(ctx), errAuthControl):
		status, reason = "failed", "invalid_control"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status, reason = "timed_out", "timed_out"
	}
	return authFailure(cmd, reg, status, reason)
}

func readAuthControl(in io.Reader, cancel context.CancelCauseFunc) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 256), 4096)
	if !scanner.Scan() {
		if scanner.Err() != nil {
			cancel(errAuthControl)
		}
		return
	}
	var control struct {
		Action string `json:"action"`
	}
	d := json.NewDecoder(strings.NewReader(scanner.Text()))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(&control) != nil || d.Decode(&extra) != io.EOF {
		cancel(errAuthControl)
		return
	}
	switch control.Action {
	case "cancel":
		cancel(context.Canceled)
	case "decline":
		cancel(errAuthDeclined)
	default:
		cancel(errAuthControl)
	}
}

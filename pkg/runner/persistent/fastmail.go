package persistent

// Fastmail's own MCP server takes the person's Fastmail API token as its
// bearer. A token is asked about before it is kept: one Fastmail refuses is
// never stored.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
)

// ErrFastmailRefused is Fastmail refusing a token.
var ErrFastmailRefused = errors.New("the token was refused by Fastmail: make an API token of the MCP type in Fastmail's settings (Settings → Privacy & Security → Connected apps & API tokens), Read only unless you want changes, and give it again")

// fastmailInitialize is an MCP initialize request, the first a client
// sends: one Fastmail answers only for a token it accepts.
const fastmailInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentry","version":"1"}}}`

// FastmailTokenAccepted asks Fastmail's MCP server whether it accepts
// token: ErrFastmailRefused when it answers that it does not, another error
// when it could not be asked or answered something else. A test replaces it.
var FastmailTokenAccepted = func(ctx context.Context, token string) error {
	return fastmailTokenAccepted(ctx, http.DefaultClient, "https://"+assistant.FastmailHost+"/mcp", token)
}

func fastmailTokenAccepted(ctx context.Context, client *http.Client, url, token string) error {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("a Fastmail API token is one word")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(fastmailInitialize))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ask Fastmail whether it accepts the token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrFastmailRefused
	case resp.StatusCode/100 == 2:
		return nil
	}
	return fmt.Errorf("ask Fastmail whether it accepts the token: it answered %s", resp.Status)
}

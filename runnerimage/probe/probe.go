// Package probe is the runner's side of the gateway health probe that
// lists tools: the probe's tool listing (agentry-cli#578). A session's driver grants a
// harness's permission request only for a tool the run's gateway serves,
// and a declared MCP server's tools are named by the server itself, at run
// time, unless its manifest filters them: so the probe, already inside the
// run network with the gateway's token, asks the gateway once what it
// serves, and the engine permits exactly that. One-shot runs never ask.
//
// Stdlib only: the package is embedded in the runner image's build context.
package probe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// InitBody is a spec-shaped MCP initialize request. A 2xx answer is
// proof the gateway is up and accepted the Bearer token (§R6 health probe).
const InitBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentry-probe","version":"1"}}}`

const (
	// maxListPages bounds the cursor walk: a gateway that never stops
	// paging is a fault, not a catalog.
	maxListPages = 50
	// maxListBody bounds one answer.
	maxListBody = 4 << 20
	// maxToolName bounds a name the probe passes on.
	maxToolName = 256
)

// ListTools completes the MCP handshake (initialize, then the
// initialized notification, carrying the session the gateway assigned)
// and walks tools/list to its last page. It returns the tool names in the
// gateway's order, without duplicates.
func ListTools(client *http.Client, url, token string) ([]string, error) {
	_, session, err := mcpPost(client, url, token, "", InitBody)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if _, _, err := mcpPost(client, url, token, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); err != nil {
		return nil, fmt.Errorf("initialized: %w", err)
	}
	var names []string
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": page + 2, "method": "tools/list", "params": params})
		if err != nil {
			return nil, err
		}
		raw, _, err := mcpPost(client, url, token, session, string(body))
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var res struct {
			Result *struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
				NextCursor string `json:"nextCursor"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		if res.Error != nil {
			return nil, fmt.Errorf("tools/list: %s", res.Error.Message)
		}
		if res.Result == nil {
			return nil, errors.New("tools/list: no result")
		}
		for _, t := range res.Result.Tools {
			if t.Name == "" || len(t.Name) > maxToolName || seen[t.Name] {
				continue
			}
			seen[t.Name] = true
			names = append(names, t.Name)
		}
		if res.Result.NextCursor == "" {
			return names, nil
		}
		cursor = res.Result.NextCursor
	}
	return nil, fmt.Errorf("tools/list: still paging after %d pages", maxListPages)
}

// mcpPost sends one JSON-RPC message over streamable HTTP and returns the
// answer's JSON-RPC body (unwrapped from an SSE frame when the gateway
// streams it; empty for a notification's 202) and the session id the
// gateway names.
func mcpPost(client *http.Client, url, token, session, body string) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("gateway answered %s", resp.Status)
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		session = s
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxListBody+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > maxListBody {
		return nil, "", fmt.Errorf("answer past %d bytes", maxListBody)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw = sseMessage(raw)
	}
	return bytes.TrimSpace(raw), session, nil
}

// sseMessage is the first event's data in an SSE body: one JSON-RPC
// answer, its data lines joined as the SSE format says.
func sseMessage(raw []byte) []byte {
	var data [][]byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), maxListBody)
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		case len(line) == 0 && len(data) > 0:
			return bytes.Join(data, []byte("\n"))
		}
	}
	return bytes.Join(data, []byte("\n"))
}

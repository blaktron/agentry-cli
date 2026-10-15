// Command hostclient stands in for the agent in the target-host live test
// (targethosts_live_test.go): in the agent container's place, on its
// network and with its gateway token, it calls the gateway's tools as an
// agent would, tries the target host's own address directly (which must
// fail: the agent has no route out), and prints what it got as
// docker-agent's events, the last one the answer the test reads.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type client struct {
	url, token, session string
	id                  int
}

func (c *client) call(method string, params any) (json.RawMessage, error) {
	c.id++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	return c.post(body, true)
}

func (c *client) notify(method string) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, _ = c.post(body, false)
}

func (c *client) post(body []byte, want bool) (json.RawMessage, error) {
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	if !want {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	var msg []byte
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				msg = []byte(strings.TrimSpace(d))
				break
			}
		}
	} else {
		msg, _ = io.ReadAll(resp.Body)
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(msg, &r); err != nil {
		return nil, fmt.Errorf("%v: %s", err, msg)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", r.Error)
	}
	return r.Result, nil
}

// step is one call the test asked for: a tool and its arguments.
type step struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

func main() {
	c := &client{url: os.Getenv("HOSTCLIENT_URL"), token: os.Getenv(os.Getenv("HOSTCLIENT_TOKEN_ENV"))}
	var steps []step
	if err := json.Unmarshal([]byte(os.Getenv("HOSTCLIENT_STEPS")), &steps); err != nil {
		fmt.Fprintln(os.Stderr, "steps:", err)
		os.Exit(2)
	}
	out := map[string]any{}
	if _, err := c.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "hostclient", "version": "1"}}); err != nil {
		fmt.Fprintln(os.Stderr, "initialize:", err)
		os.Exit(1)
	}
	c.notify("notifications/initialized")
	if r, err := c.call("tools/list", map[string]any{}); err == nil {
		out["tools"] = json.RawMessage(r)
	} else {
		out["toolsError"] = err.Error()
	}
	var results []any
	for _, s := range steps {
		r, err := c.call("tools/call", map[string]any{"name": s.Tool, "arguments": s.Args})
		if err != nil {
			results = append(results, map[string]any{"tool": s.Tool, "error": err.Error()})
			continue
		}
		results = append(results, map[string]any{"tool": s.Tool, "result": json.RawMessage(r)})
	}
	out["results"] = results
	// The agent's own route to the target host: none.
	if a := os.Getenv("HOSTCLIENT_DIAL"); a != "" {
		conn, err := net.DialTimeout("tcp", a, 3*time.Second)
		if err == nil {
			_ = conn.Close()
			out["dial"] = "connected"
		} else {
			out["dial"] = err.Error()
		}
	}
	answer, _ := json.Marshal(out)
	ev := func(v any) { b, _ := json.Marshal(v); fmt.Println(string(b)) }
	ev(map[string]any{"type": "agent_choice", "message": map[string]any{"content": string(answer)}})
	ev(map[string]any{"type": "stream_stopped", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/blaktron/agentry/pkg/apiclient"
)

// Publish progress over server-sent events (§11 "SSE/poll"; agentry#65,
// agentry-cli#26). The registry streams status snapshots for a version;
// the CLI prints stage completions as they land and the verdict when the
// stream reaches a terminal state. Anything that is not a clean stream —
// a server without the endpoint (404), a dropped connection, a stream
// that ends without a verdict — falls back to the 2 s poll, so the
// command's behaviour on older registries is unchanged.

// versionEvent mirrors the API's `event: status` payload.
type versionEvent struct {
	Version      string   `json:"version"`
	Status       string   `json:"status"`
	StatusReason *string  `json:"statusReason"`
	Stages       []string `json:"stages"`
	Done         bool     `json:"done"`
}

// errNoStream tells the caller to poll instead.
var errNoStream = errors.New("no event stream")

// streamVerdict follows the version's event stream. It returns
// (handled=true, err) when the stream delivered a verdict, and
// (false, errNoStream) when the caller should poll.
func streamVerdict(ctx context.Context, httpc *http.Client, reg, token string, out io.Writer, ns, name string, v *apiclient.AgentVersion, lookup func(context.Context) (*apiclient.AgentVersion, error)) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, publishPollBudget)
	defer cancel()
	u := strings.TrimRight(reg, "/") + "/api/v1/agents/" + url.PathEscape(ns) + "/" + url.PathEscape(name) + "/versions/" + url.PathEscape(v.Version) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, errNoStream
	}
	req.Header.Set("Accept", "text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// The stream outlives the client's normal request timeout.
	c := *httpc
	c.Timeout = 0
	res, err := c.Do(req)
	if err != nil {
		return false, errNoStream
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		return false, errNoStream
	}

	seen := map[string]bool{}
	last := string(v.Status)
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			if event == "gone" || event == "timeout" {
				return false, errNoStream
			}
			var ev versionEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				continue
			}
			for _, st := range ev.Stages {
				if !seen[st] {
					seen[st] = true
					_, _ = fmt.Fprintf(out, "  stage %s done\n", st)
				}
			}
			if ev.Status != last {
				_, _ = fmt.Fprintf(out, "status: %s\n", ev.Status)
				last = ev.Status
			}
			if ev.Done {
				// The verdict line wants the full version resource (digest etc.).
				cur, err := lookup(ctx)
				if err != nil {
					return false, errNoStream
				}
				done, verr := verdict(out, ns, name, cur)
				if !done {
					return false, errNoStream
				}
				return true, verr
			}
		case line == "":
			event = ""
		}
	}
	return false, errNoStream
}

// waitVerdict prefers the event stream and falls back to polling.
func waitVerdict(ctx context.Context, client *apiclient.ClientWithResponses, httpc *http.Client, reg, token string, out io.Writer, ns, name string, v *apiclient.AgentVersion) error {
	lookup := func(ctx context.Context) (*apiclient.AgentVersion, error) {
		resp, err := client.GetVersionInfoWithResponse(ctx, ns, name, v.Version)
		if err != nil {
			return nil, err
		}
		if resp.JSON200 == nil {
			return nil, apiProblem("poll version "+v.Version, resp.StatusCode(), resp.ApplicationproblemJSONDefault, resp.Body)
		}
		return resp.JSON200, nil
	}
	if handled, err := streamVerdict(ctx, httpc, reg, token, out, ns, name, v, lookup); handled {
		return err
	}
	// Give a registry that closed the stream early a moment before polling.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(200 * time.Millisecond):
	}
	return pollVerdict(ctx, client, out, ns, name, v)
}

package adapter

// The models a Copilot plan offers, listed by Copilot CLI itself (plan
// model-sign-ins.md §4.1, research copilot-cli.md R8; agentry-cli#808):
// setup and the desktop offer what the person's plan allows rather than a
// model id typed by hand. The list comes through Copilot CLI, never from a
// call of Agentry's own to the Copilot API, in a short-lived container that
// is a run's in all but the gateway (engine.ListCopilotModels).
//
// The order: connect; session.create with no MCP server at all, the token
// reaching Copilot CLI through the session's token callback as a run's does
// (spec.ModelSignInCallback); session.model.list. Measured against Copilot
// CLI 1.0.95 with no account: the flat models.list authenticates only with a
// token passed in its own parameters ("Not authenticated" without one), and
// takes no callback, so the list is the session's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// CopilotModel is one model a Copilot plan offers, as Copilot CLI lists it.
type CopilotModel struct {
	// ID is the model id a policy names (model.name), such as
	// claude-sonnet-5.5.
	ID string `json:"id"`
	// Name is GitHub's display name, "" when it gave none.
	Name string `json:"name,omitempty"`
	// Multiplier is the premium-request multiplier GitHub bills the model
	// at (billing.multiplier), 0 when it gave none.
	Multiplier float64 `json:"multiplier,omitempty"`
}

// copilotListedModel is the part of a session.model.list entry read here
// (copilot-sdk go/rpc/zrpc.go Model).
type copilotListedModel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Billing *struct {
		Multiplier *float64 `json:"multiplier"`
	} `json:"billing"`
	Policy *struct {
		State string `json:"state"`
	} `json:"policy"`
}

// CopilotListContainer is the container a model list runs Copilot CLI in:
// a run's (the same argv, environment, read-only root and empty home), in
// image.
func CopilotListContainer(image string) spec.ContainerSpec { return copilotContainer(image) }

// ListCopilotModels drives Copilot CLI on rw (the list container's stdio)
// and returns the models the signed-in account's plan offers, in the order
// Copilot CLI gives them. token is the sign-in, handed to Copilot CLI
// through its token callback and nowhere else. A model the account's
// policy disables is left out, and so is a model of a provider of the
// person's own (an id carrying its provider, provider/id): neither is the
// plan's to offer. ListCopilotModels closes rw.
func ListCopilotModels(ctx context.Context, rw io.ReadWriteCloser, token func(ctx context.Context, refresh bool) (SignInToken, error)) ([]CopilotModel, error) {
	if token == nil {
		return nil, errors.New("copilot models: no sign-in to list the models of")
	}
	s, err := newCopilotDriver(rw, SessionOptions{ModelSignIn: true, SignInToken: token})
	if err != nil {
		return nil, err
	}
	// The driver's events are drained so its reader never waits on them,
	// and the last error kept to explain a failure.
	said := make(chan string, 1)
	go func() {
		last := ""
		for ev := range s.Events() {
			if ev.Kind == EventError && ev.Text != "" {
				last = ev.Text
			}
		}
		said <- last
	}()
	models, err := s.listModels(ctx)
	_ = s.Close()
	if last := <-said; err != nil && last != "" && !strings.Contains(err.Error(), last) {
		err = fmt.Errorf("%w (%s)", err, last)
	}
	return models, err
}

// listModels is the list's exchange, after connect.
func (s *copilotSession) listModels(ctx context.Context) ([]CopilotModel, error) {
	if err := s.connect(ctx); err != nil {
		return nil, err
	}
	params, err := s.listSessionParams()
	if err != nil {
		return nil, err
	}
	if _, err := s.call(ctx, "session.create", params); err != nil {
		return nil, s.startError(err)
	}
	res, err := s.call(ctx, "session.model.list", map[string]any{"sessionId": s.sessionID})
	if err != nil {
		return nil, err
	}
	var got struct {
		List []json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal(res, &got); err != nil {
		return nil, fmt.Errorf("session.model.list: %w", err)
	}
	out := []CopilotModel{}
	seen := map[string]bool{}
	for _, raw := range got.List {
		var m copilotListedModel
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] || strings.Contains(id, "/") {
			continue
		}
		if m.Policy != nil && strings.EqualFold(m.Policy.State, "disabled") {
			continue
		}
		seen[id] = true
		cm := CopilotModel{ID: id, Name: strings.TrimSpace(m.Name)}
		if m.Billing != nil && m.Billing.Multiplier != nil {
			cm.Multiplier = *m.Billing.Multiplier
		}
		out = append(out, cm)
	}
	return out, nil
}

// listSessionParams is session.create's for a list: a run's switches, with
// no MCP server, no tool and the token callback's registration. No turn is
// ever sent on it.
func (s *copilotSession) listSessionParams() (map[string]any, error) {
	b, err := json.Marshal(copilotSessionConfig{
		ClientName:             agentryClientName,
		Model:                  "auto",
		AvailableTools:         []string{copilotAvailableTools},
		MCPServers:             map[string]copilotMCPServer{},
		RequestPermission:      true,
		SkipCustomInstructions: true,
		CustomAgentsLocalOnly:  true,
		SkipEmbeddingRetrieval: true,
		EmbeddingCacheStorage:  "in-memory",
		MCPOAuthTokenStorage:   "in-memory",
		EnvValueMode:           "direct",
	})
	if err != nil {
		return nil, err
	}
	var params map[string]any
	if err := json.Unmarshal(b, &params); err != nil {
		return nil, err
	}
	params["sessionId"] = s.sessionID
	params["gitHubTokenProviderRegistrationId"] = s.registrationID
	return params, nil
}

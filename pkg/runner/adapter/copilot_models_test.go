package adapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/copilottest"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// listOn runs ListCopilotModels against srv.
func listOn(t *testing.T, srv *copilottest.Server, token func(context.Context, bool) (adapter.SignInToken, error)) ([]adapter.CopilotModel, error) {
	t.Helper()
	toSrvR, toSrvW := io.Pipe()
	fromSrvR, fromSrvW := io.Pipe()
	go func() {
		_ = srv.Serve(toSrvR, fromSrvW)
		_ = fromSrvW.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return adapter.ListCopilotModels(ctx, pipeRW{Reader: fromSrvR, WriteCloser: toSrvW}, token)
}

func multiplier(x float64) map[string]any { return map[string]any{"multiplier": x} }

// A Copilot plan's models come from Copilot CLI: connect, a session with no
// MCP server and no tool whose token arrives through the callback, then the
// session's list. A model the account's policy disables, and one of a
// provider of the person's own, are not offered; no turn is ever sent.
func TestACopilotPlansModelsAreListedThroughCopilotCLI(t *testing.T) {
	srv := &copilottest.Server{Models: []map[string]any{
		{"id": "claude-sonnet-5.5", "name": "Claude Sonnet 5.5", "billing": multiplier(1), "policy": map[string]any{"state": "enabled"}},
		{"id": "claude-opus-5.5", "name": "Claude Opus 5.5", "billing": multiplier(10), "policy": map[string]any{"state": "disabled", "terms": "Ask your administrator"}},
		{"id": "gpt-5-mini", "name": "GPT-5 mini", "billing": multiplier(0)},
		{"id": "ollama/llama4", "name": "Llama 4 (your provider)"},
		{"id": "gpt-5-mini", "name": "GPT-5 mini again"},
		{"name": "no id"},
	}}
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	got, err := listOn(t, srv, k.get)
	if err != nil {
		t.Fatal(err)
	}
	want := []adapter.CopilotModel{
		{ID: "claude-sonnet-5.5", Name: "Claude Sonnet 5.5", Multiplier: 1},
		{ID: "gpt-5-mini", Name: "GPT-5 mini"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models %+v, want %+v", got, want)
	}
	create, _, tokensGiven, _, methods := srv.Snapshot()
	if strings.Join(methods, " ") != "connect session.create session.model.list" {
		t.Fatalf("methods %v", methods)
	}
	if len(tokensGiven) != 1 || tokensGiven[0]["accessToken"] != "ghu_initialtoken0000" || len(k.asked) != 1 || k.asked[0] {
		t.Fatalf("the token: given %v, asked %v", tokensGiven, k.asked)
	}
	if servers, _ := create["mcpServers"].(map[string]any); len(servers) != 0 {
		t.Fatalf("a list session has MCP servers: %v", create["mcpServers"])
	}
	if strings.Join(anyStrings(create["availableTools"]), ",") != "mcp:*" || create["enableSessionTelemetry"] != false ||
		create["requestPermission"] != true || create["enableSkills"] != false || create["gitHubTokenProviderRegistrationId"] == "" {
		t.Fatalf("session.create %v", create)
	}
	// The token reached Copilot CLI through the callback alone.
	b, _ := json.Marshal(create)
	if strings.Contains(string(b), "ghu_") {
		t.Fatalf("session.create carries the token: %s", b)
	}
}

// A list with no sign-in asks nothing of Copilot CLI; a sign-in GitHub
// refuses, or an organization's policy, is said as a run says it.
func TestACopilotModelListFailsAsARunWould(t *testing.T) {
	if _, err := adapter.ListCopilotModels(context.Background(), pipeRW{}, nil); err == nil {
		t.Fatal("listed with no sign-in")
	}
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	_, err := listOn(t, &copilottest.Server{CreateError: "Error: Access denied by policy settings"}, k.get)
	var refused *adapter.OrgPolicyRefusal
	if !errors.As(err, &refused) || refused.Policy != receipt.OrgPolicyClient {
		t.Fatalf("the CLI turned off: %v", err)
	}
	_, err = listOn(t, &copilottest.Server{CreateError: `GitHub token validation failed: 401 Unauthorized: {"message":"Bad credentials"}`}, k.get)
	if err == nil || !strings.Contains(err.Error(), "sign in again with agentry model-signins add copilot") {
		t.Fatalf("a refused token: %v", err)
	}
	k.err = errors.New("the sign-in was removed")
	if _, err = listOn(t, &copilottest.Server{}, k.get); err == nil {
		t.Fatal("listed on a token the store could not give")
	}
}

// The list runs Copilot CLI in a run's container, but for the image.
func TestTheCopilotListContainerIsARuns(t *testing.T) {
	ad, _ := adapter.Get(adapter.CopilotImageAdapterName)
	res, err := ad.Compile(copilotInput(manifest.ModeAutonomous))
	if err != nil {
		t.Fatal(err)
	}
	if got := adapter.CopilotListContainer(res.Agent.Image); !reflect.DeepEqual(got, res.Agent) {
		t.Fatalf("list container %+v, run's %+v", got, res.Agent)
	}
}

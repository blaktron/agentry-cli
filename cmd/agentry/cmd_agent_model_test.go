package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
)

// agentModelOf is the model --json agent list says the agent runs on.
func agentModelOf(t *testing.T, agent string) *agentModelOut {
	t.Helper()
	var out struct {
		Agents []agentState `json:"agents"`
	}
	if err := json.Unmarshal([]byte(cli(t, "--json", "agent", "list")), &out); err != nil {
		t.Fatal(err)
	}
	for _, st := range out.Agents {
		if st.Agent == agent {
			return st.Model
		}
	}
	t.Fatalf("agent list has no %s", agent)
	return nil
}

// configuredModel is the saved model id the agent's configuration names.
func configuredModel(t *testing.T, home, agent string) string {
	t.Helper()
	c, err := persistent.LoadConfig(home, agent)
	if err != nil {
		t.Fatal(err)
	}
	return c.Model
}

// configure --model names the saved model an agent runs on, by its name,
// and default has it follow the default again; list and show say which.
// A name the computer does not have is refused.
func TestConfigureNamesTheSavedModelAnAgentRunsOn(t *testing.T) {
	home, path := labModels(t)
	_, pol := readPolicy(t, path)
	qwen, _ := pol.FindModel("Qwen")

	if m := agentModelOf(t, "assistant"); m == nil || !m.FollowsDefault || m.SavedModel == nil || m.SavedModel.Name != "anthropic" {
		t.Fatalf("an agent that names none: %+v", m)
	}
	out := cli(t, "agent", "configure", "assistant", "--model", "Qwen")
	if !strings.Contains(out, "model: Qwen; the agent takes it when it next starts") {
		t.Fatalf("configure said:\n%s", out)
	}
	if got := configuredModel(t, home, "assistant"); got != qwen.ID {
		t.Fatalf("the configuration names %q, want Qwen's id %q", got, qwen.ID)
	}
	if m := agentModelOf(t, "assistant"); m == nil || m.FollowsDefault || m.SavedModel == nil || m.SavedModel.ID != qwen.ID || m.Model != "custom/qwen3" {
		t.Fatalf("list says %+v", m)
	}
	if show := cli(t, "agent", "show", "assistant"); !strings.Contains(show, "  model:      Qwen (custom/qwen3)\n") {
		t.Fatalf("show:\n%s", show)
	}
	if list := cli(t, "agent", "list"); !strings.Contains(list, "MODEL") || !strings.Contains(list, "Qwen") {
		t.Fatalf("list:\n%s", list)
	}

	cli(t, "agent", "configure", "assistant", "--model", "default")
	if got := configuredModel(t, home, "assistant"); got != "" {
		t.Fatalf("--model default left %q", got)
	}
	if show := cli(t, "agent", "show", "assistant"); !strings.Contains(show, "  model:      the default, now anthropic (anthropic/") {
		t.Fatalf("show:\n%s", show)
	}

	_, _, err := runCLI(t, strings.NewReader(""), "agent", "configure", "assistant", "--model", "Gemma")
	wantClassified(t, err, engine.ExitUsage)
	if !strings.Contains(err.Error(), `no saved model "Gemma"`) {
		t.Fatalf("refusal: %v", err)
	}
}

// Removing a saved model, or a provider, that a persistent agent names is
// refused with the agent listed, until --move-to moves it; nothing moves to
// the default on its own.
func TestRemovingAModelAnAgentNamesIsRefusedUntilItIsMoved(t *testing.T) {
	home, path := labModels(t)
	cli(t, "agent", "configure", "assistant", "--model", "Qwen")

	_, _, err := runCLI(t, strings.NewReader(""), "models", "remove", "Qwen")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "agent") || !strings.Contains(err.Error(), "(assistant)") || !strings.Contains(err.Error(), "--move-to") {
		t.Fatalf("refusal: %v", err)
	}
	_, _, err = runCLI(t, strings.NewReader(""), "providers", "remove", "custom-lab")
	wantClassified(t, err, engine.ExitPolicy)
	if !strings.Contains(err.Error(), "(assistant)") {
		t.Fatalf("a provider's removal: %v", err)
	}

	out := cli(t, "models", "remove", "Qwen", "--move-to", "Llama")
	if !strings.Contains(out, "moved agent") {
		t.Fatalf("remove said:\n%s", out)
	}
	_, pol := readPolicy(t, path)
	llama, _ := pol.FindModel("Llama")
	if got := configuredModel(t, home, "assistant"); got != llama.ID {
		t.Fatalf("the agent names %q after the move, want Llama's %q", got, llama.ID)
	}

	cli(t, "providers", "remove", "custom-lab", "--move-to", "anthropic")
	_, pol = readPolicy(t, path)
	anthropic, _ := pol.FindModel("anthropic")
	if got := configuredModel(t, home, "assistant"); got != anthropic.ID {
		t.Fatalf("the agent names %q after its provider was removed, want %q", got, anthropic.ID)
	}
}

package main

// The saved model a persistent agent runs on: how list and show say it,
// how configure --model names it, the check a segment's start makes, and
// the agents a model's removal moves.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/persistent"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// agentModelDefault is what configure --model takes to have the agent
// follow the default.
const agentModelDefault = "default"

// agentModelOut is the saved model an agent runs on, as list and show say
// it: the one it names, or the default it follows, with its provider and
// model id; or why its next turn would be refused for it.
type agentModelOut struct {
	SavedModel     *spec.SavedModelRef `json:"savedModel,omitempty"`
	Model          string              `json:"model,omitempty"`
	FollowsDefault bool                `json:"followsDefault,omitempty"`
	Refusal        string              `json:"refusal,omitempty"`
}

// readAgentModel is the saved model agent a, configured as cfg, runs on
// under the policy at policyPath (the default policy for "").
func readAgentModel(a persistent.Agent, cfg persistent.Config, policyPath string) *agentModelOut {
	out := &agentModelOut{FollowsDefault: cfg.Model == ""}
	pol, _, err := modelsPolicy(policyPath)
	if err != nil {
		out.Refusal = err.Error()
		return out
	}
	rm, err := localrun.ChooseRunModel(pol, cfg.Model, "")
	if err != nil {
		if localrun.IsMissingModel(err) {
			out.Refusal = fmt.Sprintf("it names the saved model %q, which this computer does not have: move it to one it has (agentry agent configure %s --model <name>)", cfg.Model, a.Name)
		} else {
			out.Refusal = err.Error()
		}
		return out
	}
	if rm.Saved.ID != "" {
		out.SavedModel = &spec.SavedModelRef{ID: rm.Saved.ID, Name: rm.Saved.Name}
	}
	out.Model = rm.Policy.Model.Provider + "/" + rm.Policy.Model.Name
	return out
}

// line is the model as show prints it.
func (m *agentModelOut) line() string {
	switch {
	case m.Refusal != "":
		return "refused: " + safeTerminalText(m.Refusal, 300)
	case m.SavedModel == nil:
		return m.Model
	case m.FollowsDefault:
		return fmt.Sprintf("the default, now %s (%s)", safeTerminalText(m.SavedModel.Name, 64), m.Model)
	}
	return fmt.Sprintf("%s (%s)", safeTerminalText(m.SavedModel.Name, 64), m.Model)
}

// short is the model as list prints it.
func (m *agentModelOut) short() string {
	switch {
	case m == nil:
		return "-"
	case m.Refusal != "":
		return "refused"
	case m.SavedModel == nil:
		return m.Model
	case m.FollowsDefault:
		return "default (" + safeTerminalText(m.SavedModel.Name, 64) + ")"
	}
	return safeTerminalText(m.SavedModel.Name, 64)
}

// agentModelRef is the saved model configure --model names, as the id the
// agent's configuration keeps: "" for the default.
func agentModelRef(ref, policyPath string) (string, policy.SavedModel, error) {
	ref = strings.TrimSpace(ref)
	if strings.EqualFold(ref, agentModelDefault) {
		return "", policy.SavedModel{}, nil
	}
	pol, _, err := modelsPolicy(policyPath)
	if err != nil {
		return "", policy.SavedModel{}, err
	}
	sm, ok := pol.FindModel(ref)
	if !ok {
		return "", sm, engine.Classify(engine.ExitUsage, fmt.Errorf("--model: no saved model %q on this computer (agentry models list), or default to follow the default", ref))
	}
	return sm.ID, sm, nil
}

// agentModelUsable is the check a segment of agent makes before it starts
// on a saved model: its key in its variable or the keychain, its sign-in
// kept.
func agentModelUsable(home, agent string) func(localrun.RunModel) error {
	other := "move the agent to another saved model (agentry agent configure " + agent + " --model <name>; agentry models list)"
	return func(rm localrun.RunModel) error {
		return savedModelUsable(home, rm, os.Getenv, other)
	}
}

// modelAgents are the persistent agents on this computer that name one of
// the saved models ids.
func modelAgents(home string, ids map[string]bool) ([]modelUser, []persistent.Agent) {
	var users []modelUser
	var named []persistent.Agent
	for _, a := range persistent.AgentsIn(home) {
		if a.DesktopOnly {
			continue
		}
		cfg, err := persistent.LoadConfig(home, a.Name)
		if err != nil || cfg.Model == "" || !ids[cfg.Model] {
			continue
		}
		users = append(users, modelUser{Kind: "agent", ID: a.Name, Name: a.DisplayName(cfg)})
		named = append(named, a)
	}
	return users, named
}

// moveAgents points each agent at the saved model to: a stopped one's
// configuration under its lock, a running one's through its supervisor,
// which takes it at its next turn.
func moveAgents(ctx context.Context, home string, named []persistent.Agent, to policy.SavedModel) error {
	for _, a := range named {
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		id := to.ID
		_, _, err := persistent.Configure(cctx, home, a, persistent.ClientCLI, persistent.Change{Model: &id})
		cancel()
		if err != nil {
			return fmt.Errorf("move the %s agent to %s: %w", a.Name, to.Name, err)
		}
	}
	return nil
}

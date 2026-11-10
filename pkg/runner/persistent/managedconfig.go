package persistent

import (
	"fmt"

	"github.com/blaktron/agentry/pkg/managedsettings"
	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/managed"
)

// ManagedField is one field of an agent's configuration an organization's
// managed settings set: the row, the field it sets, the organization, and
// whether it is locked or a default.
type ManagedField struct {
	ID     string `json:"id"`
	Field  string `json:"field"`
	Org    string `json:"org"`
	Locked bool   `json:"locked"`
}

// The fields of a configuration a managed setting may set.
const (
	FieldName        = "name"
	FieldPersonality = "personality"
	FieldRunOnStart  = "runOnStart"
)

// isAssistant says a is the Assistant built into Agentry.
func isAssistant(a Agent) bool { return !a.Published() && a.Name == managed.AgentAssistant }

// ManagedConfig is c as the held managed settings configure the agent a:
// a locked field replaces the person's value, and a default fills one the
// person has not set (a name or a personality left empty; whether it
// starts with Agentry, until the agent first starts). The person's file
// keeps their own values: what this returns is read, never saved. It also
// names each field a setting set.
func ManagedConfig(a Agent, c Config, held managed.Effective) (Config, []ManagedField) {
	if !isAssistant(a) {
		return c, nil
	}
	var out []ManagedField
	if f := held.Text(managedsettings.AgentsAssistantName); f.Set {
		if f.Locked || c.DisplayName == "" {
			if name, err := cleanName(f.Value); err == nil {
				c.DisplayName = name
			}
		}
		out = append(out, ManagedField{ID: managedsettings.AgentsAssistantName, Field: FieldName, Org: f.Org, Locked: f.Locked})
	}
	if f := held.Text(managedsettings.AgentsAssistantPersonality); f.Set {
		if f.Locked || c.Personality == "" {
			c.Personality = f.Value
		}
		out = append(out, ManagedField{ID: managedsettings.AgentsAssistantPersonality, Field: FieldPersonality, Org: f.Org, Locked: f.Locked})
	}
	if f := held.Toggle(managedsettings.AgentsAssistantRunOnStart); f.Set {
		if f.Locked || (c.Segments == 0 && !c.RunOnStart) {
			c.RunOnStart = f.Value
		}
		out = append(out, ManagedField{ID: managedsettings.AgentsAssistantRunOnStart, Field: FieldRunOnStart, Org: f.Org, Locked: f.Locked})
	}
	return c, out
}

// managedChangeRefusal refuses a change to a field of the agent a that a
// held managed setting locks.
func managedChangeRefusal(a Agent, ch Change, held managed.Effective) error {
	if !isAssistant(a) {
		return nil
	}
	if ch.Name != nil {
		if err := held.Text(managedsettings.AgentsAssistantName).Refusal("The Assistant's name", managedsettings.AgentsAssistantName); err != nil {
			return err
		}
	}
	if ch.Personality != nil {
		if err := held.Text(managedsettings.AgentsAssistantPersonality).Refusal("The Assistant's personality", managedsettings.AgentsAssistantPersonality); err != nil {
			return err
		}
	}
	if ch.RunOnStart != nil {
		if err := held.Toggle(managedsettings.AgentsAssistantRunOnStart).Refusal("Whether the Assistant starts with Agentry", managedsettings.AgentsAssistantRunOnStart); err != nil {
			return err
		}
	}
	return nil
}

// isCodingAgent says a is the Coding agent built into Agentry.
func isCodingAgent(a Agent) bool { return !a.Published() && a.Name == managed.AgentCoder }

// reachRows are the servers and packs rows that configure the agent a, or
// "" for an agent no row configures.
func reachRows(a Agent) (servers, packs string) {
	switch {
	case isAssistant(a):
		return managedsettings.AgentsAssistantServers, managedsettings.AgentsAssistantKnowledge
	case isCodingAgent(a):
		return managedsettings.AgentsCodingAgentServers, managedsettings.AgentsCodingAgentKnowledge
	}
	return "", ""
}

// managedServers are the remote MCP servers the held managed settings put
// on the agent a, as the person's own remote servers are written.
func managedServers(a Agent, held managed.Effective) []agentServer {
	id, _ := reachRows(a)
	if id == "" {
		return nil
	}
	f := held.Servers(id)
	var out []agentServer
	for _, s := range f.Value {
		out = append(out, agentServer{Server: manifest.MCPServer{URL: s.URL, Auth: manifest.MCPAuth(s.Auth)}, Org: f.Org, Locked: f.Locked})
	}
	return out
}

// managedPacks are the organization's shared knowledge packs the held
// managed settings put on the agent a.
func managedPacks(a Agent, held managed.Effective) managed.Field[[]string] {
	_, id := reachRows(a)
	if id == "" {
		return managed.Field[[]string]{}
	}
	return held.Packs(id)
}

// lockedSwitchRefusal refuses switching off an entry an organization's
// managed settings lock on the agent.
func lockedSwitchRefusal(r Roster, id string, on bool) error {
	if on {
		return nil
	}
	if e, ok := r.Entry(id); ok && e.Locked {
		return fmt.Errorf("%s is locked on by %s's managed settings and cannot be switched off", e.Name, e.Organization)
	}
	return nil
}

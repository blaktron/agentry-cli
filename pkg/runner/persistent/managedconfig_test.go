package persistent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/managedsettings"

	"github.com/blaktron/agentry-cli/pkg/managed"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// holdSettings holds a document of acme's with settings.
func holdSettings(t *testing.T, settings map[string]managedsettings.Setting) {
	t.Helper()
	doc := managedsettings.Document{Schema: managedsettings.Schema, Registry: "https://registry.example", Org: "acme", Version: 9,
		IssuedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC), Settings: settings}
	prev := currentManaged
	t.Cleanup(func() { currentManaged = prev })
	currentManaged = func() managed.Effective {
		return managed.Effective{Documents: []managed.State{{Registry: doc.Registry, Document: doc}}}
	}
}

func setting(t *testing.T, v any, locked bool) managedsettings.Setting {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return managedsettings.Setting{Value: b, Locked: locked}
}

// An organization's lock names the Assistant and gives it a personality
// whatever the person set; a default fills only what the person left
// empty, and starts it with Agentry until it first starts. Another agent
// is not configured, and the person's file keeps their own values.
func TestManagedSettingsConfigureTheAssistant(t *testing.T) {
	holdSettings(t, map[string]managedsettings.Setting{
		managedsettings.AgentsAssistantName:        setting(t, "Ada", true),
		managedsettings.AgentsAssistantPersonality: setting(t, "Brief and kind.", false),
		managedsettings.AgentsAssistantRunOnStart:  setting(t, true, false),
	})
	a := assistantAgent(t)
	own := Config{DisplayName: "Max", Personality: "Chatty."}
	got, fields := ManagedConfig(a, own, currentManaged())
	if got.DisplayName != "Ada" || got.Personality != "Chatty." || !got.RunOnStart || len(fields) != 3 {
		t.Fatalf("configured %+v, fields %+v", got, fields)
	}
	if own.DisplayName != "Max" {
		t.Fatal("the person's config was changed")
	}
	if got, _ := ManagedConfig(a, Config{}, currentManaged()); got.Personality != "Brief and kind." {
		t.Fatalf("a default personality did not fill an empty one: %q", got.Personality)
	}
	if got, _ := ManagedConfig(a, Config{Segments: 4}, currentManaged()); got.RunOnStart {
		t.Fatal("a runOnStart default changed an agent that has started")
	}
	if name, personality := effectivePersona(a, own); name != "Ada" || personality != "Chatty." {
		t.Fatalf("persona %q %q", name, personality)
	}
	if a.DisplayName(own) != "Ada" {
		t.Fatalf("display name %q", a.DisplayName(own))
	}
	if got, fields := ManagedConfig(coderAgent(t), own, currentManaged()); got.DisplayName != "Max" || fields != nil {
		t.Fatalf("the Coding agent was configured: %+v %v", got, fields)
	}
}

// A change to a field an organization locks is refused, naming the
// organization and the setting; a default's field is the person's to
// change.
func TestAChangeToALockedFieldIsRefused(t *testing.T) {
	holdSettings(t, map[string]managedsettings.Setting{
		managedsettings.AgentsAssistantName:        setting(t, "Ada", true),
		managedsettings.AgentsAssistantPersonality: setting(t, "Brief.", false),
	})
	home := shortHome(t)
	a := assistantAgent(t)
	c := Config{Enabled: true}
	name := "Max"
	if _, err := ApplyChange(home, a, &c, Change{Name: &name}); err == nil || !strings.Contains(err.Error(), "managed by acme's managed settings (agents.assistant.name)") {
		t.Fatalf("a locked name changed: %v", err)
	}
	p := "Chatty."
	if _, err := ApplyChange(home, a, &c, Change{Personality: &p}); err != nil {
		t.Fatalf("a default personality's change: %v", err)
	}
}

// An organization's servers and shared packs are on the agent's roster as
// the organization's: asked about at the next start as the person's own
// are, on with no switch when locked, and switchable as a default.
func TestManagedServersAndPacksAreOnTheRoster(t *testing.T) {
	servers := []managedsettings.Server{{Name: "tickets", URL: "https://mcp.acme.example/tickets", Auth: "none"}}
	holdSettings(t, map[string]managedsettings.Setting{
		managedsettings.AgentsAssistantServers:   setting(t, servers, true),
		managedsettings.AgentsAssistantKnowledge: setting(t, []string{"handbook"}, false),
	})
	home := shortHome(t)
	a := assistantAgent(t)
	c := Config{Enabled: true, Switches: map[string]bool{}}
	r := BuildRoster(a, c, nil)
	var srv RosterEntry
	for _, e := range r {
		if e.Kind == KindServer && e.Organization == "acme" {
			srv = e
		}
	}
	if srv.ID == "" || srv.GrantedBy != GrantedByOrganization || !srv.Locked || !srv.On || !strings.Contains(srv.Line, "acme's managed settings") {
		t.Fatalf("the organization's server %+v", srv)
	}
	pack, ok := r.Entry(KnowledgeEntryID("handbook"))
	if !ok || pack.GrantedBy != GrantedByOrganization || pack.Locked || !pack.On {
		t.Fatalf("the organization's pack %+v (%v)", pack, ok)
	}
	pending := Unconsented(r, c)
	asked := map[string]bool{}
	for _, e := range pending {
		asked[e.ID] = true
	}
	if !asked[srv.ID] || !asked[pack.ID] {
		t.Fatalf("not asked about at the start: %v", asked)
	}

	// A locked server's switch is refused off, and a switch in the file
	// does not take it off.
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: srv.ID, On: false}}); err == nil || !strings.Contains(err.Error(), "locked on by acme's managed settings") {
		t.Fatalf("a locked server switched off: %v", err)
	}
	c.Switches[srv.ID] = false
	c.Switches[pack.ID] = false
	r = BuildRoster(a, c, nil)
	if e, _ := r.Entry(srv.ID); !e.On {
		t.Fatal("a file switch took a locked server off")
	}
	if e, _ := r.Entry(pack.ID); e.On {
		t.Fatal("a default pack ignored the person's switch")
	}

	// Apply opens the organization's pack when it is on.
	c.Switches[pack.ID] = true
	RecordConsent(BuildRoster(a, c, nil), &c)
	var opened []string
	open := func(_ context.Context, names []string) ([]engine.AttachedPack, func(), error) {
		opened = names
		return nil, func() {}, nil
	}
	_, _, done, err := ApplyWith(context.Background(), home, a, c, BuildRoster(a, c, nil), Openers{Packs: open}, nil, fakeSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	done()
	if len(opened) != 1 || opened[0] != "handbook" {
		t.Fatalf("opened %v", opened)
	}
}

// An organization's server that signs in by OAuth is on the roster as the
// organization's, locked on, and says it signs in with the person's own
// connection, as the person's own does.
func TestAManagedOAuthServerSignsInWithThePersonsConnection(t *testing.T) {
	holdSettings(t, map[string]managedsettings.Setting{
		managedsettings.AgentsAssistantServers: setting(t, []managedsettings.Server{{Name: "crm", URL: "https://mcp.crm.example", Auth: "oauth"}}, true),
	})
	for _, e := range BuildRoster(assistantAgent(t), Config{Enabled: true}, nil) {
		if e.Organization == "acme" && e.Kind == KindServer {
			if !e.On || !e.Locked || !strings.Contains(e.Line, "agentry agent connect assistant") {
				t.Fatalf("the OAuth server %+v", e)
			}
			return
		}
	}
	t.Fatal("no organization server on the roster")
}

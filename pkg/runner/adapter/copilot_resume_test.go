package adapter_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/copilottest"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// A persistent agent's Copilot CLI keeps its sessions on the agent's session
// store, a volume given to the image's user, instead of the tmpfs home; any
// other run's stays in the home.
func TestAPersistentCopilotKeepsItsHomeOnTheSessionStore(t *testing.T) {
	ad, _ := adapter.Get(adapter.CopilotImageAdapterName)
	in := copilotInput(manifest.ModeInteractive)
	in.Persistent = true
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	home := ""
	for _, e := range res.Agent.Env {
		if e.Name == "COPILOT_HOME" {
			home = e.Value
		}
	}
	want := []spec.Mount{{Source: spec.PHSession, Target: spec.SessionMount}}
	if home != spec.SessionMount || !slices.Equal(res.Session.Mounts, want) || res.Session.StoreOwner != "copilot" {
		t.Fatalf("COPILOT_HOME %q, session %+v", home, res.Session)
	}

	res, err = ad.Compile(copilotInput(manifest.ModeInteractive))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Agent.Env {
		if e.Name == "COPILOT_HOME" && e.Value == spec.SessionMount {
			t.Fatal("an interactive run's COPILOT_HOME is a session store")
		}
	}
	if len(res.Session.Mounts) != 0 || res.Session.StoreOwner != "" {
		t.Fatalf("an interactive run's session %+v", res.Session)
	}
}

// A segment given its stored session resumes it with the session's
// configuration as it is now, the gateway's new bearer and the token
// callback included, and creates nothing.
func TestACopilotSessionResumesTheStoredOne(t *testing.T) {
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	srv := &copilottest.Server{Stored: []string{"stored-1"}}
	d := drive(t, srv, adapter.SessionOptions{GatewayTools: []string{"write_report"}, ModelSignIn: true, SignInToken: k.get,
		GatewayToken: "gateway-bearer-0002", ContinueID: "stored-1", Persistent: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	c, ok := d.drv.(adapter.Continuing)
	if !ok {
		t.Fatal("the Copilot driver does not continue conversations")
	}
	if id, done := c.ConversationID(), first(c.Continued()); id != "stored-1" || !done {
		t.Fatalf("conversation %q continued %v", id, done)
	}
	if _, err := d.drv.Prompt(ctx, "and again"); err != nil {
		t.Fatal(err)
	}
	resume := srv.Resumed()
	gw := resume["mcpServers"].(map[string]any)["gateway"].(map[string]any)
	sys, _ := resume["systemMessage"].(map[string]any)
	if resume["sessionId"] != "stored-1" || gw["headers"].(map[string]any)["Authorization"] != "Bearer gateway-bearer-0002" ||
		sys["content"] != "You are a QuickAgent." || resume["gitHubTokenProviderRegistrationId"] == nil {
		t.Fatalf("session.resume %v", resume)
	}
	_, _, _, _, methods := srv.Snapshot()
	if slices.Contains(methods, "session.create") {
		t.Fatalf("a resumed session was created too: %v", methods)
	}
}

// A session Copilot CLI cannot find leaves a new one in its place, and the
// reason; a first segment, given none, creates one and says nothing.
func TestACopilotSessionItCannotResumeIsCreatedAnew(t *testing.T) {
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	srv := &copilottest.Server{}
	d := drive(t, srv, adapter.SessionOptions{GatewayTools: []string{"write_report"}, ModelSignIn: true, SignInToken: k.get,
		ContinueID: "lost-1", Persistent: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	c := d.drv.(adapter.Continuing)
	done, why := c.Continued()
	create, _, _, _, _ := srv.Snapshot()
	if done || !strings.Contains(why, "Session not found") || c.ConversationID() == "lost-1" || create["sessionId"] != c.ConversationID() {
		t.Fatalf("continued %v %q, conversation %q, created %v", done, why, c.ConversationID(), create["sessionId"])
	}
}

// An organization's policy refusing the resume is that outcome, never a
// new session.
func TestACopilotResumeRefusedByPolicyIsNamed(t *testing.T) {
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	srv := &copilottest.Server{Stored: []string{"stored-1"}, ResumeError: "Access denied by policy settings"}
	d := drive(t, srv, adapter.SessionOptions{ModelSignIn: true, SignInToken: k.get, ContinueID: "stored-1", Persistent: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := d.drv.Start(ctx)
	var refused *adapter.OrgPolicyRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("Start = %v, want the organization's refusal", err)
	}
	if create, _, _, _, _ := srv.Snapshot(); create != nil {
		t.Fatalf("a session was created after the refusal: %v", create)
	}
}

func first(b bool, _ string) bool { return b }

// A Copilot session compacts its history in place on request
// (session.history.compact) and says when Copilot CLI did not, or does not
// serve the call.
func TestACopilotSessionCompactsOnRequest(t *testing.T) {
	for _, c := range []struct {
		name    string
		compact map[string]any
		ok      bool
	}{
		{"compacted", map[string]any{"success": true, "tokensRemoved": 41000, "messagesRemoved": 12}, true},
		{"not compacted", map[string]any{"success": false, "tokensRemoved": 0, "messagesRemoved": 0}, false},
		{"not served", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := &tokens{expires: time.Now().Add(8 * time.Hour)}
			srv := &copilottest.Server{Compact: c.compact}
			d := drive(t, srv, adapter.SessionOptions{GatewayTools: []string{"write_report"}, ModelSignIn: true, SignInToken: k.get, Persistent: true})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := d.drv.Start(ctx); err != nil {
				t.Fatal(err)
			}
			err := d.drv.(adapter.Compacting).CompactConversation(ctx)
			if (err == nil) != c.ok {
				t.Fatalf("CompactConversation = %v", err)
			}
		})
	}
}

package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// A persistent agent's Claude Code keeps its transcript, in its config
// directory's projects folder, which is the agent's session store mounted
// over the sign-in's volume, and is told its conversation on the command
// line; every other run's Claude Code keeps nothing.
func TestAPersistentClaudeKeepsItsTranscriptOnTheSessionStore(t *testing.T) {
	ad, _ := Get(ClaudeImageAdapterName)
	in := claudeSignInInput(manifest.ModeInteractive)
	in.Persistent = true
	r, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.Agent.Argv, "--no-session-persistence") {
		t.Fatalf("a persistent agent's argv %q keeps no transcript", r.Agent.Argv)
	}
	want := []spec.Mount{{Source: spec.PHSession, Target: ClaudeImageConfigDir + "/projects"}}
	ca := r.Session.ConversationArgs
	if !slices.Equal(r.Session.Mounts, want) || r.Session.StoreOwner != "node" || ca == nil || ca.New != "--session-id" || ca.Continue != "--resume" {
		t.Fatalf("session %+v, conversation args %+v", r.Session, ca)
	}
	if r.ModelSignIn == nil || r.ModelSignIn.Path != ClaudeImageConfigDir {
		t.Fatalf("the sign-in moved: %+v", r.ModelSignIn)
	}

	r, err = ad.Compile(claudeSignInInput(manifest.ModeInteractive))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.Agent.Argv, "--no-session-persistence") || r.Session.ConversationArgs != nil || len(r.Session.Mounts) != 0 {
		t.Fatalf("an interactive run keeps its conversation: argv %q session %+v", r.Agent.Argv, r.Session)
	}
}

// claudeHandshake plays Claude Code's side of a persistent segment's start
// on stdio: the initialize request answered, or, when gone, the error result
// Claude Code prints for a conversation --resume cannot find, and its exit.
func claudeHandshake(stdio *fakeStdio, gone bool, failure string) {
	if gone || failure != "" {
		if failure == "" {
			failure = "No conversation found with session ID: c-1"
		}
		stdio.say(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["` + failure + `"]}`)
		_ = stdio.Close()
		return
	}
	for line := range stdio.lines {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(line, &m) == nil && m.Type == "control_request" {
			stdio.say(`{"type":"control_response","response":{"subtype":"success","request_id":"` + m.RequestID + `","response":{}}}`)
		}
	}
}

// A persistent segment's Claude Code is asked to initialize before its
// first turn: answered, the segment is in the conversation it named; ended
// with "No conversation found", Start says the stored conversation could not
// be continued. A run's session sends nothing before its first turn.
func TestAClaudeSegmentKnowsWhetherItsConversationContinued(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ad, _ := Get(ClaudeImageAdapterName)
	for _, tc := range []struct {
		name                 string
		opts                 SessionOptions
		gone                 bool
		failure              string
		wantStartError       bool
		wantID               string
		wantContinued        bool
		wantNotContinued     bool
		wantNoInitialRequest bool
	}{
		{name: "new", opts: SessionOptions{Persistent: true, ConversationID: "n-1"}, wantID: "n-1"},
		{name: "continued", opts: SessionOptions{Persistent: true, ContinueID: "c-1"}, wantID: "c-1", wantContinued: true},
		{name: "gone", opts: SessionOptions{Persistent: true, ContinueID: "c-1"}, gone: true, wantNotContinued: true},
		{name: "a run", opts: SessionOptions{}, wantNoInitialRequest: true},
		{name: "another failure", opts: SessionOptions{Persistent: true, ContinueID: "c-1"}, failure: "Invalid API key", wantStartError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdio := newFakeStdio()
			drv, err := ad.Session(ctx, stdio, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			go claudeHandshake(stdio, tc.gone, tc.failure)
			err = drv.Start(ctx)
			var nc *NotContinuedError
			switch {
			case tc.wantNotContinued:
				if !errors.As(err, &nc) || !strings.Contains(nc.Reason, "No conversation found") {
					t.Fatalf("Start = %v, want the stored conversation not continued", err)
				}
				return
			case tc.wantStartError:
				if err == nil || errors.As(err, &nc) || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("Start = %v, want the start's own failure, not a conversation that did not continue", err)
				}
				return
			case err != nil:
				t.Fatalf("Start: %v", err)
			}
			stdio.mu.Lock()
			wrote := stdio.writes.String()
			stdio.mu.Unlock()
			if tc.wantNoInitialRequest {
				if wrote != "" {
					t.Fatalf("a run's Start wrote %q", wrote)
				}
				return
			}
			if !strings.Contains(wrote, `"subtype":"initialize"`) {
				t.Fatalf("Start wrote %q, not Claude Code's initialize request", wrote)
			}
			c := drv.(Continuing)
			if done, why := c.Continued(); c.ConversationID() != tc.wantID || done != tc.wantContinued || why != "" {
				t.Fatalf("conversation %q continued %v %q", c.ConversationID(), done, why)
			}
			_ = drv.Close()
		})
	}
}

// A persistent agent's codex keeps CODEX_HOME on the session store whatever
// its token route, as the restart route already does.
func TestAPersistentCodexKeepsItsHomeOnTheSessionStore(t *testing.T) {
	ad, _ := Get(CodexImageAdapterName)
	in := codexInput()
	in.Mode, in.Persistent = manifest.ModeInteractive, true
	r, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	home := ""
	for _, e := range r.Agent.Env {
		if e.Name == "CODEX_HOME" {
			home = e.Value
		}
	}
	want := []spec.Mount{{Source: spec.PHSession, Target: spec.SessionMount}}
	if home != spec.SessionMount || !slices.Equal(r.Session.Mounts, want) || r.Session.StoreOwner != "codex" || r.Session.RestartOnRenewal {
		t.Fatalf("CODEX_HOME %q, session %+v", home, r.Session)
	}
}

// codexThreads plays codex's app-server for a segment's start: resume finds
// only stored, and every thread method it was asked is recorded.
func codexThreads(stdio *fakeStdio, stored string, asked chan<- string) {
	for line := range stdio.lines {
		var m struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(line, &m) != nil || m.ID == nil {
			continue
		}
		id := itoa(*m.ID)
		switch m.Method {
		case "initialize":
			stdio.say(`{"id":` + id + `,"result":{"userAgent":"codex"}}`)
		case "thread/resume":
			asked <- "resume " + m.Params["threadId"].(string)
			if m.Params["threadId"] != stored {
				stdio.say(`{"id":` + id + `,"error":{"code":-32600,"message":"no rollout found for thread id ` + m.Params["threadId"].(string) + `"}}`)
				continue
			}
			stdio.say(`{"id":` + id + `,"result":{"thread":{"id":"` + stored + `"}}}`)
		case "thread/start":
			b, _ := json.Marshal(m.Params["ephemeral"])
			asked <- "start ephemeral=" + string(b)
			stdio.say(`{"id":` + id + `,"result":{"thread":{"id":"t-new"}}}`)
		}
	}
	close(asked)
}

// A persistent segment's codex resumes the stored thread; one it cannot
// load leaves a kept new thread and the reason; a first segment's thread is
// kept for the next.
func TestACodexSegmentContinuesItsThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ad, _ := Get(CodexImageAdapterName)
	for _, tc := range []struct {
		name, continueID string
		wantAsked        []string
		wantID           string
		wantContinued    bool
		wantWhy          string
	}{
		{"continued", "t-old", []string{"resume t-old"}, "t-old", true, ""},
		{"lost", "t-lost", []string{"resume t-lost", "start ephemeral=false"}, "t-new", false, "no rollout found"},
		{"first", "", []string{"start ephemeral=false"}, "t-new", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdio := newFakeStdio()
			drv, err := ad.Session(ctx, stdio, SessionOptions{Persistent: true, ContinueID: tc.continueID})
			if err != nil {
				t.Fatal(err)
			}
			asked := make(chan string, 8)
			go codexThreads(stdio, "t-old", asked)
			if err := drv.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			c := drv.(Continuing)
			done, why := c.Continued()
			_ = drv.Close()
			var got []string
			for a := range asked {
				got = append(got, a)
			}
			if !slices.Equal(got, tc.wantAsked) || c.ConversationID() != tc.wantID || done != tc.wantContinued ||
				(tc.wantWhy == "") != (why == "") || !strings.Contains(why, tc.wantWhy) {
				t.Fatalf("asked %v, conversation %q continued %v %q", got, c.ConversationID(), done, why)
			}
		})
	}
}

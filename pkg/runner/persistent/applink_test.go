package persistent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
	"github.com/blaktron/agentry/pkg/manifest"
)

func call(t *testing.T, home, name, tool, args string) applink.Answer {
	t.Helper()
	a, err := applink.Call(context.Background(), AppSocketPath(home, name), applink.Request{Tool: tool, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestSelfConfigureWritesNameAndPersonalityOnly: the Assistant's app link
// socket is served for its segment, which is given its path; self_configure
// saves the name and the personality and nothing else, refuses an argument
// it does not know, and is not given again at the next turn; every call is a
// line of the segment's applink.log; a tool outside the Assistant's set is
// refused.
func TestSelfConfigureWritesNameAndPersonalityOnly(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv := startAssistant(t, home, st)
	defer func() { _, _ = sv.Stop(context.Background()) }()
	if st.segments[0].Grants.AppLink != AppSocketPath(home, "assistant") {
		t.Fatalf("the segment's app link socket: %q", st.segments[0].Grants.AppLink)
	}
	if err := os.MkdirAll(filepath.Join(st.segments[0].OutDir, "receipts"), 0o700); err != nil {
		t.Fatal(err)
	}
	before, _ := LoadConfig(home, "assistant")
	if a := call(t, home, "assistant", applink.SelfConfigure, `{"name":"Sandra","share":"/etc"}`); !a.IsError {
		t.Fatalf("an argument it does not know was taken: %+v", a)
	}
	a := call(t, home, "assistant", applink.SelfConfigure, `{"name":"Sandra","personality":"dry, brief"}`)
	if a.IsError || !strings.Contains(a.Text, `"Sandra"`) {
		t.Fatalf("self_configure: %+v", a)
	}
	after, _ := LoadConfig(home, "assistant")
	if after.DisplayName != "Sandra" || after.Personality != "dry, brief" {
		t.Fatalf("config after: %+v", after)
	}
	after.DisplayName, after.Personality, after.PersonaGiven = before.DisplayName, before.Personality, before.PersonaGiven
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	if string(b1) != string(b2) {
		t.Errorf("self_configure changed more than the name and personality:\n%s\n%s", b1, b2)
	}
	if err := sv.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if sent := st.last.sentTurns(); len(sent) != 1 || sent[0] != "hello" {
		t.Errorf("the name it saved itself was given to it again: %q", sent)
	}
	if a := call(t, home, "assistant", "agent_stop", `{"agent":"assistant"}`); !a.IsError {
		t.Errorf("a tool outside the Assistant's set was answered: %+v", a)
	}
	log, err := os.ReadFile(filepath.Join(st.segments[0].OutDir, "receipts", appLinkLog))
	if err != nil || strings.Count(string(log), "\n") != 3 || !strings.Contains(string(log), `"tool":"self_configure"`) {
		t.Errorf("applink.log: %q (%v)", log, err)
	}
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AppSocketPath(home, "assistant")); !os.IsNotExist(err) {
		t.Error("the app link socket outlived the supervisor")
	}
}

// TestMainsToolsAreTheDesktopsToAnswer: Main runs only under the desktop's
// supervisor, whose handler answers Main's tools; without one, a tool is
// answered as the desktop's.
func TestMainsToolsAreTheDesktopsToAnswer(t *testing.T) {
	home := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a, err := Lookup("main")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true}
	RecordConsent(BuildRoster(a, c, nil), &c)
	if err := SaveConfig(home, "main", c); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(context.Background(), Options{Home: home, Agent: a, Client: ClientCLI, Policy: policy.Default()}); err == nil {
		t.Fatal("the CLI started Main")
	}
	var asked []applink.Request
	st := &starter{}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: a, Client: ClientDesktop, Policy: policy.Default(), Docker: &dockerx.Fake{}, StartSegment: st.start,
		AppLink: func(_ context.Context, req applink.Request) applink.Answer {
			asked = append(asked, req)
			return applink.Answer{Text: "3 QuickAgents found"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = sv.Stop(context.Background()) }()
	if got := call(t, home, "main", "registry_search", `{"query":"summarise PDFs"}`); got.Text != "3 QuickAgents found" || len(asked) != 1 {
		t.Fatalf("registry_search: %+v, asked %v", got, asked)
	}
	if got := call(t, home, "main", applink.SelfConfigure, `{"name":"x"}`); !got.IsError {
		t.Errorf("self_configure, not Main's, was answered: %+v", got)
	}
}

// TestTheAssistantsHelperToolsGoToItsClient: the Assistant's set holds
// Agentry Helper's tools, which its supervisor hands to its client's handler,
// as Main's does; self_configure is still the supervisor's own. Run from the
// CLI with no handler, a Helper tool is answered as Agentry Desktop's.
func TestTheAssistantsHelperToolsGoToItsClient(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	var asked []applink.Request
	st := &starter{resumed: true}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientDesktop, Policy: policy.Default(), Docker: &dockerx.Fake{}, StartSegment: st.start,
		AppLink: func(_ context.Context, req applink.Request) applink.Answer {
			asked = append(asked, req)
			return applink.Answer{Text: "answered " + req.Tool}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range applink.Tools(applink.SetMain) {
		if got := call(t, home, "assistant", tool, `{}`); got.IsError || got.Text != "answered "+tool {
			t.Errorf("%s: %+v", tool, got)
		}
	}
	if len(asked) != len(applink.Tools(applink.SetMain)) {
		t.Errorf("the handler was asked %d times", len(asked))
	}
	if got := call(t, home, "assistant", applink.SelfConfigure, `{"name":"Sandra"}`); got.IsError || len(asked) != len(applink.Tools(applink.SetMain)) {
		t.Errorf("self_configure went to the handler: %+v", got)
	}
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	cli := startAssistant(t, home, &starter{resumed: true})
	defer func() { _, _ = cli.Stop(context.Background()) }()
	got := call(t, home, "assistant", "registry_search", `{"query":"summarise PDFs"}`)
	if !got.IsError || !strings.Contains(got.Text, "Agentry Desktop") {
		t.Errorf("registry_search with no handler: %+v", got)
	}
}

// TestThePublisherToolsAreTheAssistantsAlone: the Assistant's supervisor hands
// each publisher tool to its client's handler, and answers it as Agentry
// Desktop's when run from the CLI with no handler; Agentry Helper's refuses
// them without asking its handler.
func TestThePublisherToolsAreTheAssistantsAlone(t *testing.T) {
	publisher := applink.Tools(applink.Compose(applink.PartPublisher))
	home := shortHome(t)
	enable(t, home)
	var asked []applink.Request
	st := &starter{resumed: true}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientDesktop, Policy: policy.Default(), Docker: &dockerx.Fake{}, StartSegment: st.start,
		AppLink: func(_ context.Context, req applink.Request) applink.Answer {
			asked = append(asked, req)
			return applink.Answer{Text: "answered " + req.Tool}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range publisher {
		if got := call(t, home, "assistant", tool, `{}`); got.IsError || got.Text != "answered "+tool {
			t.Errorf("%s: %+v", tool, got)
		}
	}
	if len(asked) != len(publisher) {
		t.Errorf("the handler was asked %d times, want %d", len(asked), len(publisher))
	}
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	cli := startAssistant(t, home, &starter{resumed: true})
	got := call(t, home, "assistant", applink.QuickAgentSubmit, `{"folder":"share/quickagents/alice/x"}`)
	if !got.IsError || !strings.Contains(got.Text, "Agentry Desktop") {
		t.Errorf("quickagent_submit with no handler: %+v", got)
	}
	if _, err := cli.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	mainHome := shortHome(t)
	t.Setenv("HOME", t.TempDir())
	a, err := Lookup("main")
	if err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true}
	RecordConsent(BuildRoster(a, c, nil), &c)
	if err := SaveConfig(mainHome, "main", c); err != nil {
		t.Fatal(err)
	}
	var mainAsked []applink.Request
	msv, err := Start(context.Background(), Options{
		Home: mainHome, Agent: a, Client: ClientDesktop, Policy: policy.Default(), Docker: &dockerx.Fake{}, StartSegment: (&starter{}).start,
		AppLink: func(_ context.Context, req applink.Request) applink.Answer {
			mainAsked = append(mainAsked, req)
			return applink.Answer{Text: "answered " + req.Tool}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = msv.Stop(context.Background()) }()
	for _, tool := range publisher {
		if got := call(t, mainHome, "main", tool, `{}`); !got.IsError || !strings.Contains(got.Text, "no such tool") {
			t.Errorf("Agentry Helper's %s: %+v", tool, got)
		}
	}
	if len(mainAsked) != 0 {
		t.Errorf("Agentry Helper's handler was asked %v", mainAsked)
	}
}

// TestAPublishedAgentIsNeverGivenThePublisherTools: a published agent that
// declares every key the Assistant does composes its set from them alone,
// even were it to carry the Assistant's Builtin, so no release lists the
// publisher tools.
func TestAPublishedAgentIsNeverGivenThePublisherTools(t *testing.T) {
	caps := &manifest.Persistent{Clock: true, Memory: true, SelfConfigure: true, Helper: true, Meetings: true}
	for _, a := range []Agent{
		{Name: "acme.helper", Release: &localrun.Installed{}, Caps: caps},
		{Name: "acme.helper", Release: &localrun.Installed{}, Caps: caps, Builtin: localrun.Assistant()},
	} {
		if set := a.AppLinkWith(Config{}); applink.Has(set, applink.PartPublisher) || !applink.Has(set, applink.PartHelper) {
			t.Errorf("a published agent's app link is %q", set)
		}
	}
	if set := assistantAgent(t).AppLinkWith(Config{}); !applink.Has(set, applink.PartPublisher) {
		t.Errorf("the Assistant's app link is %q", set)
	}
}

// TestAFetchedFolderIsCheckedBeforeTheAnswer: once the client's handler has
// answered quickagent_fetch or quickagent_revise without an error, the
// supervisor has the share's check run on the folder the call wrote, and on
// nothing else, before the answer reaches the agent, so the agent reads the
// files in the same turn. A failed call, a tool that writes nothing into the
// share, and arguments that name no QuickAgent's folder check nothing; a
// check that cannot take the files says so in the answer.
func TestAFetchedFolderIsCheckedBeforeTheAnswer(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true, checksShare: true}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientDesktop, Policy: policy.Default(), Docker: &dockerx.Fake{}, StartSegment: st.start,
		AppLink: func(_ context.Context, req applink.Request) applink.Answer {
			if strings.Contains(string(req.Arguments), "refused") {
				return applink.Answer{Text: "the person said no", IsError: true}
			}
			return applink.Answer{Text: "done"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = sv.Stop(context.Background()) }()
	for _, c := range []struct{ tool, args, folder string }{
		{applink.QuickAgentFetch, `{"ref":"alice/notes@1.2.0"}`, "quickagents/alice/notes"},
		{applink.QuickAgentFetch, `{"ref":"acme/report-writer"}`, "quickagents/acme/report-writer"},
		{applink.QuickAgentRevise, `{"folder":"share/quickagents/alice/notes/","instructions":"x","where":"computer"}`, "quickagents/alice/notes"},
		{applink.QuickAgentRevise, `{"folder":"quickagents/alice/notes","instructions":"x","where":"agentry"}`, "quickagents/alice/notes"},
		{applink.QuickAgentFetch, `{"ref":"alice/refused"}`, ""},
		{applink.QuickAgentSubmit, `{"folder":"share/quickagents/alice/notes"}`, ""},
		{applink.QuickAgentManage, `{"ref":"alice/notes","action":"retire"}`, ""},
		{applink.QuickAgentFetch, `{"ref":"../etc"}`, ""},
		{applink.QuickAgentFetch, `{"ref":"alice/../../x"}`, ""},
		{applink.QuickAgentRevise, `{"folder":"share/quickagents/alice/../../etc","instructions":"x","where":"computer"}`, ""},
		{applink.QuickAgentRevise, `{"folder":"share/notes","instructions":"x","where":"computer"}`, ""},
	} {
		before := len(st.checking.checked())
		got := call(t, home, "assistant", c.tool, c.args)
		checked := st.checking.checked()[before:]
		switch {
		case c.folder == "" && len(checked) != 0:
			t.Errorf("%s %s checked %v", c.tool, c.args, checked)
		case c.folder != "" && (len(checked) != 1 || checked[0] != c.folder):
			t.Errorf("%s %s checked %v, want %s", c.tool, c.args, checked, c.folder)
		}
		if c.folder != "" && (got.IsError || got.Text != "done") {
			t.Errorf("%s %s answered %+v", c.tool, c.args, got)
		}
	}
	st.checking.mu.Lock()
	st.checking.err = errors.New("1 more file(s) in the share wait for the next turn's check")
	st.checking.mu.Unlock()
	got := call(t, home, "assistant", applink.QuickAgentFetch, `{"ref":"alice/notes"}`)
	if got.IsError || !strings.HasPrefix(got.Text, "done") || !strings.Contains(got.Text, "next turn") {
		t.Errorf("a check that could not take every file: %+v", got)
	}
}

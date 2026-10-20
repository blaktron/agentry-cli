package persistent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
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
	if a := call(t, home, "assistant", "quickagent_run", `{"ref":"x/y"}`); !a.IsError {
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

package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// TestASessionPermitsWhatItsAttachmentsGrant (agentry-cli#576): knowledge
// and a Writing Voice are attached at run time and never move the plan,
// so the session's driver, which grants a harness's permission request
// only for a tool the gateway serves, must be told the run's own grant,
// not the compiled plan's, which lacks them. The harness here asks through
// ACP, as docker-agent does, where a denial fails the whole turn.
func TestASessionPermitsWhatItsAttachmentsGrant(t *testing.T) {
	attached := []string{"knowledge_list", "knowledge_search", "knowledge_read", "voice_read"}
	for _, tc := range []struct {
		name   string
		attach bool
		want   string
	}{
		{"attached", true, "a"},
		{"none attached", false, "r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, f := sessionFixture(t)
			happyFake(f)
			h := &fakeHarness{asks: attached}
			f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
			if tc.attach {
				p.Knowledge = []AttachedPack{AttachPack(makePack(t, "style-guide"))}
				p.Voice = attachTestVoice(t)
			}
			s, err := StartSession(context.Background(), f, p)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if err := s.Send(context.Background(), "write it up"); err != nil {
				t.Fatalf("send: %v", err)
			}
			if _, err := s.End(context.Background(), false); err != nil {
				t.Fatalf("end: %v", err)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if !h.granted {
				t.Error("the plan's write_report was not granted")
			}
			for _, tool := range attached {
				if got := h.answers[tool]; got != tc.want {
					t.Errorf("%s: the runner answered %q, want %q", tool, got, tc.want)
				}
			}
		})
	}
}

// TestASessionRefusesTheWebTools: the web tools are an autonomous draft's.
// A session refuses a run that brings them, before any run directory
// exists, which is what keeps web_read and web_search off a session's
// grant now that the grant is read from the run's own tools spec
// (agentry-cli#576).
func TestASessionRefusesTheWebTools(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	p.Web = &WebRun{Scan: toolsrv.WebScanJev, JevKey: testJevKey}
	if _, err := StartSession(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "web tools do not run in an interactive session") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.RunRoot, "runs", "5e5510")); !os.IsNotExist(err) {
		t.Fatalf("a refused session left a run directory (%v)", err)
	}
}

// TestASessionPermitsTheGatewaysDeclaredServers (agentry-cli#578): a
// declared MCP server's tools are named by the server at run time unless
// its manifest filters them, and docker-agent asks permission for each one
// not marked read-only. The session's probe lists what the gateway serves,
// and the driver permits that beside the run's own grant; a tool the
// gateway did not list is still refused.
func TestASessionPermitsTheGatewaysDeclaredServers(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	f.Foreground["-probe"] = &dockerx.FakeForeground{Handler: probeListing("create_entities", "read_graph")}
	h := &fakeHarness{asks: []string{"create_entities", "shell"}}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Send(context.Background(), "remember this"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := s.End(context.Background(), false); err != nil {
		t.Fatalf("end: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.granted {
		t.Error("the run's own write_report was not granted")
	}
	if got := h.answers["create_entities"]; got != "a" {
		t.Errorf("a tool the gateway serves: the runner answered %q, want allow_once", got)
	}
	if got := h.answers["shell"]; got != "r" {
		t.Errorf("a tool the gateway does not serve: the runner answered %q, want reject_once", got)
	}
}

// TestOnlyASessionsProbeListsTools: the listing is a session's alone (a
// one-shot run has no permission round-trip), and a session whose probe
// cannot list the gateway's tools does not start: it would refuse tools
// its contract grants.
func TestOnlyASessionsProbeListsTools(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	var argv []string
	f.Foreground["-probe"].Before = func(r dockerx.RunSpec) { argv = r.Argv }
	if _, err := Run(context.Background(), f, p); err != nil {
		t.Fatalf("run: %v", err)
	}
	if slices.Contains(argv, "--list-tools") {
		t.Errorf("a one-shot run's probe lists tools: %v", argv)
	}

	sp, sf := sessionFixture(t)
	happyFake(sf)
	sf.Foreground["-probe"] = &dockerx.FakeForeground{ExitCode: 0}
	if _, err := StartSession(context.Background(), sf, sp); err == nil || !strings.Contains(err.Error(), "list") {
		t.Fatalf("a session whose probe listed nothing: %v", err)
	}
}

// TestTheProbesListingIsReadStrictly: only {"tools":[…]} with sane names
// is a listing; anything else, or more than the bound, is an error.
func TestTheProbesListingIsReadStrictly(t *testing.T) {
	if got, err := parseServedTools([]byte(`{"tools":["a","b"]}` + "\n")); err != nil || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("a listing: %v, %v", got, err)
	}
	for _, bad := range []string{"", "gateway ok", `{"tools":["a"]}` + "\n" + `{"tools":["b"]}`, `{"tools":null}`, `{"names":["a"]}`, `{"tools":[""]}`, `{"tools":["` + strings.Repeat("x", 257) + `"]}`} {
		if _, err := parseServedTools([]byte(bad)); err == nil {
			t.Errorf("%q was read as a listing", bad)
		}
	}
	var b probeOutput
	_, _ = b.Write([]byte(`{"tools":[`))
	if n, err := b.Write(make([]byte, maxProbeListing)); n != maxProbeListing || err != nil {
		t.Errorf("a write past the bound was not taken whole: %d, %v", n, err)
	}
	if b.Bytes() != nil {
		t.Error("a listing past its bound was kept")
	}
	if got := permitted([]string{"write_report"}, []string{"create_entities", "write_report"}); !slices.Equal(got, []string{"write_report", "create_entities"}) {
		t.Errorf("permitted = %v", got)
	}
}

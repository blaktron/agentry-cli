package engine

import (
	"context"
	"os"
	"path/filepath"
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

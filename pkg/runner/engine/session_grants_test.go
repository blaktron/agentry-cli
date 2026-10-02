package engine

import (
	"context"
	"io"
	"slices"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// recordingSessionAdapter is the default adapter whose Session records the
// tools the engine permits the driver to grant.
type recordingSessionAdapter struct {
	adapter.Adapter
	got *[]string
}

func (a recordingSessionAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, opts adapter.SessionOptions) (adapter.SessionDriver, error) {
	*a.got = append([]string(nil), opts.GatewayTools...)
	return a.Adapter.Session(ctx, rw, opts)
}

// TestASessionPermitsWhatItsAttachmentsGrant (agentry-cli#576): knowledge
// and a Writing Voice are attached at run time and never move the plan,
// so a session's driver, which grants a harness's permission request only
// for a tool it is told the gateway serves, must be told the run's own
// grants — not the compiled plan's, which lack them. Under ACP
// (docker-agent) and codex a tool left off the list is denied.
func TestASessionPermitsWhatItsAttachmentsGrant(t *testing.T) {
	attached := []string{"knowledge_list", "knowledge_search", "knowledge_read", "voice_read"}
	start := func(t *testing.T, attach bool) []string {
		t.Helper()
		p, f := sessionFixture(t)
		happyFake(f)
		h := &fakeHarness{}
		f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
		var got []string
		p.Adapter = recordingSessionAdapter{Adapter: p.Adapter, got: &got}
		if attach {
			p.Knowledge = []AttachedPack{AttachPack(makePack(t, "style-guide"))}
			p.Voice = attachTestVoice(t)
		}
		s, err := StartSession(context.Background(), f, p)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, err := s.End(context.Background(), false); err != nil {
			t.Fatalf("end: %v", err)
		}
		for _, tool := range p.Plan.Tools {
			if !slices.Contains(got, tool) {
				t.Errorf("the session does not permit the plan's %s: %v", tool, got)
			}
		}
		return got
	}
	t.Run("attached", func(t *testing.T) {
		got := start(t, true)
		for _, tool := range attached {
			if !slices.Contains(got, tool) {
				t.Errorf("a session with a pack and a voice attached does not permit %s: %v", tool, got)
			}
		}
	})
	t.Run("none attached", func(t *testing.T) {
		got := start(t, false)
		for _, tool := range attached {
			if slices.Contains(got, tool) {
				t.Errorf("a session with nothing attached permits %s: %v", tool, got)
			}
		}
	})
}

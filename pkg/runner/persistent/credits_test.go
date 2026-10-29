package persistent

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/inference/inferencetest"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// startOnCredits starts the Assistant under a policy that pays with Agentry
// credits at reg.
func startOnCredits(t *testing.T, home string, st *starter, reg *inferencetest.Registry) *Supervisor {
	t.Helper()
	u, _ := url.Parse(reg.URL)
	pol := policy.Default()
	pol.Model = policy.Model{Provider: policy.ProviderAgentry, Name: "agentry", BaseURL: reg.URL + policy.AgentryInferencePath, ProxyAllow: []string{u.Host}}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: pol,
		Docker: &dockerx.Fake{}, StartSegment: st.start,
		Credits: inference.New(reg.URL, func() (string, error) { return inferencetest.SignIn, nil }, nil),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return sv
}

// An agent that pays with credits opens one session for its life: every
// segment, the one a restart of its turn loop starts included, is handed
// the same token with no renewal or end of its own; the supervisor renews
// it without restarting anything, and ends it once when the agent stops.
func TestAPersistentAgentKeepsOneCreditsTokenAcrossItsSegments(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	reg := inferencetest.New(t)
	reg.RenewEverySeconds = 1
	st := &starter{}
	sv := startOnCredits(t, home, st, reg)
	sv.turn.Lock()
	err := sv.nextSegment(context.Background())
	sv.turn.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1300 * time.Millisecond)
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := reg.Opened(); len(got) != 1 || got[0] != inference.PurposeAgent {
		t.Fatalf("opened %v, want one agent session", got)
	}
	st.mu.Lock()
	segs := append([]string{}, func() []string {
		var out []string
		for _, s := range st.segments {
			if s.Inference == nil {
				out = append(out, "<none>")
				continue
			}
			if s.Inference.Renew != nil || s.Inference.End != nil {
				t.Errorf("a segment was handed the session's renewal or end")
			}
			out = append(out, s.Inference.Token)
		}
		return out
	}()...)
	st.mu.Unlock()
	if len(segs) != 2 || segs[0] != inferencetest.TokenFor(1) || segs[1] != inferencetest.TokenFor(1) {
		t.Fatalf("segments' tokens %v, want the first session's twice", segs)
	}
	if len(reg.Renewed()) < 1 {
		t.Fatal("the agent's session was not renewed")
	}
	if got := reg.Ended(); len(got) != 1 || got[0] != "s-1" {
		t.Fatalf("ended %v, want s-1 once", got)
	}
}

// A session the registry ended (the computer slept past its hour) is
// opened again, and the agent's running segment restarts on the new token.
func TestAPersistentAgentReopensAnEndedCreditsSession(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	reg := inferencetest.New(t)
	reg.RenewEverySeconds = 1
	reg.EndRenewals = true
	st := &starter{}
	sv := startOnCredits(t, home, st, reg)
	deadline := time.Now().Add(5 * time.Second)
	for len(reg.Opened()) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	reg.SetEndRenewals(false)
	time.Sleep(100 * time.Millisecond)
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.segments) < 2 || st.segments[len(st.segments)-1].Inference.Token != inferencetest.TokenFor(2) {
		var toks []string
		for _, s := range st.segments {
			toks = append(toks, s.Inference.Token)
		}
		t.Fatalf("segments' tokens %v, want the last on the second session's", toks)
	}
}

// Without a sign-in an agent that pays with credits does not start.
func TestAPersistentAgentOnCreditsNeedsASignIn(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	pol := policy.Default()
	pol.Model = policy.Model{Provider: policy.ProviderAgentry, Name: "agentry", BaseURL: "https://r.example" + policy.AgentryInferencePath, ProxyAllow: []string{"r.example:443"}}
	_, err := Start(context.Background(), Options{Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: pol,
		Docker: &dockerx.Fake{}, StartSegment: (&starter{}).start})
	if err == nil || !strings.Contains(err.Error(), "agentry login") {
		t.Fatalf("started signed out: %v", err)
	}
}

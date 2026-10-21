package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// TestStageWords: each step of a start reads the same for a person in every
// client.
func TestStageWords(t *testing.T) {
	for _, c := range []struct {
		st   Stage
		want string
	}{
		{Stage{Name: receipt.StageCompile}, "Preparing"},
		{Stage{Name: StageSignIn}, "Signing in to your model"},
		{Stage{Name: receipt.StageImages}, "Getting images ready"},
		{Stage{Name: receipt.StageMCPServers, Subject: "office-word-mcp-server"}, "Installing office-word-mcp-server"},
		{Stage{Name: receipt.StageMCPServers}, "Installing your servers"},
		{Stage{Name: receipt.StagePrepare}, "Preparing"},
		{Stage{Name: receipt.StageInfra}, "Starting the gateway"},
		{Stage{Name: receipt.StageAgent, Subject: "Claude Code"}, "Starting Claude Code"},
		{Stage{Name: receipt.StageAgent}, "Starting the agent"},
		{Stage{Name: receipt.StageAgent, Detail: StageDetailResume, Subject: "Codex"}, "Picking up the conversation"},
		{Stage{Name: "later"}, "Later"},
		{Stage{}, "Starting"},
	} {
		if got := c.st.Words(); got != c.want {
			t.Errorf("%+v.Words() = %q, want %q", c.st, got, c.want)
		}
	}
}

// TestANilStageHookDoesNothing: a start with no hook reports to no one.
func TestANilStageHookDoesNothing(t *testing.T) {
	ReportStage(nil, Stage{Name: receipt.StageInfra})
}

// TestAPanickingStageHookCannotSkipTheTeardown: a hook that panics does not
// unwind the start, so a start that then fails still removes the gateway
// and the proxy it made.
func TestAPanickingStageHookCannotSkipTheTeardown(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	p.OnStage = func(Stage) { panic("a client's bug") }
	delete(f.Foreground, "-agent")
	if s, err := StartSession(context.Background(), f, p); err == nil {
		_, _ = s.End(context.Background(), false)
		t.Fatal("a start whose harness exits at once did not fail")
	}
	for _, name := range []string{"gateway", "proxy"} {
		removed := false
		for _, op := range f.Ops() {
			if op == "RemoveForce:"+containerName(p.RunID, name) {
				removed = true
			}
		}
		if !removed {
			t.Errorf("the failed start left its %s: %v", name, f.Ops())
		}
	}
}

// stageRecorder records each stage reported, with how many engine
// operations had run when it was.
type stageRecorder struct {
	mu     sync.Mutex
	f      *dockerx.Fake
	stages []Stage
	opsAt  []int
}

func (r *stageRecorder) hook(st Stage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stages = append(r.stages, st)
	r.opsAt = append(r.opsAt, len(r.f.Ops()))
}

// ranBefore says an operation naming part ran before the i'th stage.
func (r *stageRecorder) ranBefore(i int, part string) bool {
	for _, op := range r.f.Ops()[:r.opsAt[i]] {
		if strings.Contains(op, part) {
			return true
		}
	}
	return false
}

// TestASegmentStartReportsItsStagesInOrder: a segment's start reports each
// of the engine's steps once, in order, each before the step's work: the
// gateway is not started before the infra stage, nor the harness before the
// agent stage; the agent stage names the harness, and a segment that
// continues its conversation then reports the resume.
func TestASegmentStartReportsItsStagesInOrder(t *testing.T) {
	p, f, _ := persistentFixture(t, "s0")
	rec := &stageRecorder{f: f}
	p.OnStage = rec.hook
	runSegment(t, p, f)

	var got []string
	for _, st := range rec.stages {
		got = append(got, st.Name+"/"+st.Detail+"/"+st.Subject)
	}
	want := []string{"prepare//", "infra//", "agent//", "agent/resume/"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	if rec.ranBefore(1, containerName(p.RunID, "gateway")) {
		t.Errorf("the gateway started before the infra stage: %v", f.Ops()[:rec.opsAt[1]])
	}
	if !rec.ranBefore(2, containerName(p.RunID, "gateway")) || rec.ranBefore(2, containerName(p.RunID, "agent")) {
		t.Errorf("the agent stage was not between the gateway and the harness: %v", f.Ops()[:rec.opsAt[2]])
	}
}

// TestANewConversationReportsNoResume: a segment with no stored
// conversation starts its harness and picks nothing up.
func TestANewConversationReportsNoResume(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	rec := &stageRecorder{f: f}
	p.OnStage = rec.hook
	runSegment(t, p, f)
	for _, st := range rec.stages {
		if st.Detail == StageDetailResume {
			t.Errorf("a new conversation reported a resume: %+v", rec.stages)
		}
	}
	if n := len(rec.stages); n != 3 || rec.stages[n-1].Name != receipt.StageAgent {
		t.Errorf("stages = %+v, want prepare, infra, agent", rec.stages)
	}
}

package persistent

import (
	"context"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// TestTheSupervisorCarriesOnStageToTheSegment: the hook a client sets on
// the supervisor is the one each segment's start reports to, and a
// supervisor with none gives the segment none.
func TestTheSupervisorCarriesOnStageToTheSegment(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	var got []Stage
	st := &starter{}
	reporting := func(ctx context.Context, seg localrun.Segment) (engine.PersistentSession, error) {
		engine.ReportStage(seg.OnStage, engine.Stage{Name: receipt.StageImages})
		return st.start(ctx, seg)
	}
	sv, err := Start(context.Background(), Options{
		Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: policy.Default(),
		Docker: &dockerx.Fake{}, StartSegment: reporting, OnStage: func(s Stage) { got = append(got, s) },
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := sv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Words() != "Getting images ready" {
		t.Fatalf("stages = %+v, want the segment's one", got)
	}

	sv = startAssistant(t, home, st)
	defer func() { _, _ = sv.Stop(context.Background()) }()
	if last := st.segments[len(st.segments)-1]; last.OnStage != nil {
		t.Error("a supervisor with no hook gave its segment one")
	}
}

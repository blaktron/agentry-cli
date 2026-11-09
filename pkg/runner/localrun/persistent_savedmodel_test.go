package localrun

import (
	"context"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// A segment compiled from the view of the policy a saved model reads runs
// on that model, on the harness the agent was given, and its plan, which
// the receipt is written from, names the saved model.
func TestASegmentsPlanNamesTheSavedModelItRunsOn(t *testing.T) {
	pol := policy.Default()
	if err := pol.AddProvider("custom-lab", policy.Provider{Name: "Lab", BaseURL: "http://192.0.2.10:8080/v1", ProxyAllow: []string{"192.0.2.10:8080"}}); err != nil {
		t.Fatal(err)
	}
	qwen, err := pol.AddModel(policy.SavedModel{Name: "Qwen", Provider: "custom-lab", Model: "qwen3", ContextWindow: 32768})
	if err != nil {
		t.Fatal(err)
	}
	rm, err := ChooseRunModel(pol, qwen.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	seg := Segment{Builtin: Assistant(), Home: t.TempDir(), Policy: rm.Policy, PolicySHA: "s", Adapter: ImageAdapter(rm.Adapter), Build: "test"}
	sp, err := seg.Plan(context.Background(), nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if sm := sp.Plan.Harness.SavedModel; sm == nil || sm.ID != qwen.ID || sm.Name != "Qwen" {
		t.Fatalf("the segment's plan names %+v", sp.Plan.Harness.SavedModel)
	}
	if sp.Plan.Harness.Adapter != adapter.DefaultName || len(sp.Plan.Proxy.Allow) != 1 || sp.Plan.Proxy.Allow[0] == "api.anthropic.com:443" {
		t.Fatalf("the segment runs on %s, reaching %v", sp.Plan.Harness.Adapter, sp.Plan.Proxy.Allow)
	}

	// The default's view names the default.
	def, err := ChooseRunModel(pol, "", "")
	if err != nil {
		t.Fatal(err)
	}
	seg.Policy, seg.Adapter = def.Policy, ""
	if sp, err = seg.Plan(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if sm := sp.Plan.Harness.SavedModel; sm == nil || sm.ID != pol.DefaultModel().ID {
		t.Fatalf("the default's segment names %+v", sm)
	}
}

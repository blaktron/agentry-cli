package copilotmodels

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/gateway"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage"
)

// TestLiveCopilotModelList lists through the real Copilot CLI image, the
// real runner image's proxy and the engine's list container, with a token
// GitHub has never issued: Copilot CLI asks for it through the callback,
// reaches api.github.com through the proxy, and GitHub refuses it, which
// the list says as a run says a refused sign-in. Without an account that
// is as far as a list goes; a list against an account needs one. It
// builds the runner and Copilot images when absent, so it runs only when
// asked: AGENTRY_LIVE_COPILOT=1 on a host with docker and the network.
func TestLiveCopilotModelList(t *testing.T) {
	if os.Getenv("AGENTRY_LIVE_COPILOT") != "1" {
		t.Skip("drives the real Copilot CLI image: set AGENTRY_LIVE_COPILOT=1")
	}
	ctx := t.Context()
	d := dockerx.New()
	if err := d.Available(ctx); err != nil {
		t.Skipf("no docker: %v", err)
	}
	pol := policy.Default()
	runner := pol.RunnerImageTag(runnerimage.ContentVersion())
	var log strings.Builder
	if err := gateway.EnsureInstalled(ctx, d, pol, runner, t.TempDir(), &log, false); err != nil {
		t.Fatalf("runner image: %v\n%s", err, log.String())
	}
	h, _ := pol.Images.HarnessImageFor(adapter.CopilotImageAdapterName)
	if err := gateway.EnsureHarnessImage(ctx, d, adapter.CopilotImageAdapterName, h, &log); err != nil {
		t.Fatalf("copilot image: %v\n%s", err, log.String())
	}
	const fake = "ghu_livecheckmodels00000000000000000"
	var stderr strings.Builder
	start := time.Now()
	models, err := engine.ListCopilotModels(ctx, d, engine.CopilotModelsParams{
		Policy: pol, CopilotPlan: policy.CopilotPlanIndividual, RunnerImage: runner,
		SignIn: &engine.ModelSignInGrant{Kind: policy.KeyKindCopilotSignIn,
			Token: engine.ConnectionToken{AccessToken: fake, ExpiresAt: time.Now().Add(8 * time.Hour)}},
		Stderr: &stderr,
	})
	t.Logf("after %s: models %v, err %v\nstderr: %s", time.Since(start).Round(time.Millisecond), models, err, stderr.String())
	if err == nil || !strings.Contains(err.Error(), "sign in again with agentry model-signins add copilot") {
		t.Fatalf("want GitHub to refuse the made-up token, got %v", err)
	}
	if strings.Contains(err.Error(), fake) {
		t.Fatalf("the error carries the token: %v", err)
	}
	// Nothing of the list is left behind.
	if left, _ := d.PSByLabel(ctx, "agentry.run"); len(left) > 0 {
		for _, name := range left {
			if strings.HasSuffix(name, "-models") || strings.HasSuffix(name, "-proxy") {
				t.Fatalf("left %v", left)
			}
		}
	}
}

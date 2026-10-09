package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const (
	probeSignInRefresh = "PROBE-REFRESH-model-signin-i3"
	probeSignInAccess  = "probe-access-token-model-signin"
)

// fakePlanVendor issues probeSignInAccess for probeSignInRefresh.
type fakePlanVendor struct{ refreshes int }

func (v *fakePlanVendor) Refresh(_ context.Context, _ modelsignin.Record, rt string) (modelsignin.Refreshed, error) {
	if rt != probeSignInRefresh {
		return modelsignin.Refreshed{}, errors.New("unknown refresh token")
	}
	v.refreshes++
	return modelsignin.Refreshed{AccessToken: probeSignInAccess, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (v *fakePlanVendor) Revoke(context.Context, modelsignin.Record, string) error { return nil }

// panicKeys fails the test the moment anything opens the keychain.
type panicKeys struct{ t *testing.T }

func (k panicKeys) Get(string) (string, error) { k.t.Fatal("the keychain was read"); return "", nil }
func (k panicKeys) Set(string, string) error   { k.t.Fatal("the keychain was written"); return nil }
func (k panicKeys) Delete(string) error        { k.t.Fatal("the keychain was written"); return nil }

func signInPlan(kind, delivery string) *spec.Plan {
	return &spec.Plan{Harness: spec.HarnessSpec{ModelSignIn: &spec.ModelSignInSpec{
		Kind: kind, Provider: policy.SignInProvider(kind), Delivery: delivery, Path: spec.ModelSignInMount + "/" + spec.ModelSignInTokenFile,
	}}}
}

// A hosted run never spends a model sign-in (I5): each hosted driver's
// marker refuses the plan by policy, naming the kind, before the person's
// store or keychain is opened.
func TestAModelSignInNeverReachesAHostedRun(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	prev := connectionKeys
	connectionKeys = func() connection.Keychain { return panicKeys{t} }
	t.Cleanup(func() { connectionKeys = prev })
	for name, opts := range map[string]runOpts{
		"connections file": {connectionsFile: "/host/connections.json"},
		"session protocol": {sessionProtocol: true},
		"run grant":        {},
	} {
		if name == "run grant" {
			t.Setenv(envRunGrant, "grant")
		}
		if !hostedRun(opts) {
			t.Fatalf("%s: not taken for a hosted run", name)
		}
		home, _ := runnerHome()
		g, renew, err := runModelSignIn(context.Background(), home, signInPlan(policy.KeyKindChatGPTPlan, spec.ModelSignInToken), hostedRun(opts))
		var ce *engine.ClassifiedError
		if g != nil || renew != nil || !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), "a hosted run never spends a model sign-in") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	t.Setenv(envRunGrant, "")
	if hostedRun(runOpts{}) {
		t.Fatal("a run on this computer was taken for a hosted one")
	}
	// A key plan asks nothing of the store, hosted or not.
	if g, _, err := runModelSignIn(context.Background(), "", &spec.Plan{}, true); g != nil || err != nil {
		t.Fatalf("a key plan: %v %v", g, err)
	}
}

// On this computer, a plan's sign-in is the person's kept grant, refreshed
// now: the run is handed the access token alone, and renews through the
// same store (I3). With none kept the run is refused by policy with the
// signin-missing reason.
func TestARunIsHandedItsSignInsAccessTokenAlone(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AGENTRY_RUNNER_HOME", t.TempDir())
	prev := connectionKeys
	connectionKeys = func() connection.Keychain { return nil }
	t.Cleanup(func() { connectionKeys = prev })
	home, _ := runnerHome()
	plan := signInPlan(policy.KeyKindChatGPTPlan, spec.ModelSignInToken)

	_, _, err := runModelSignIn(context.Background(), home, plan, false)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || receipt.ReasonOf(err) == nil || receipt.ReasonOf(err).Code != receipt.ReasonSignInMissing {
		t.Fatalf("no sign-in kept: %v", err)
	}

	v := &fakePlanVendor{}
	t.Cleanup(modelsignin.RegisterVendor(modelsignin.ChatGPT, v))
	if _, err := modelsignin.Open(home, nil).Put(modelsignin.ChatGPT, modelsignin.Record{Label: "person@example.com"}, modelsignin.Grant{RefreshToken: probeSignInRefresh}); err != nil {
		t.Fatal(err)
	}
	g, renew, err := runModelSignIn(context.Background(), home, plan, false)
	if err != nil || g.Kind != policy.KeyKindChatGPTPlan || g.Token.AccessToken != probeSignInAccess || g.ConfigDir != "" || renew == nil {
		t.Fatalf("grant %+v, renew %v: %v", g, renew != nil, err)
	}
	b, _ := json.Marshal(g)
	if strings.Contains(string(b), probeSignInRefresh) || strings.Contains(string(b), "person@example.com") {
		t.Fatalf("the run's grant carries more than the access token: %s", b)
	}
	if t2, err := renew(context.Background()); err != nil || t2.AccessToken != probeSignInAccess || v.refreshes != 2 {
		t.Fatalf("renew: %+v %v (%d refreshes)", t2, err, v.refreshes)
	}

	out, _, err := runCLI(t, nil, "model-signins", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []modelSignInOut
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].Vendor != "chatgpt" || rows[0].Kind != policy.KeyKindChatGPTPlan || rows[0].Account != "person@example.com" {
		t.Fatalf("list = %s (%v)", out, err)
	}
	if strings.Contains(out, probeSignInRefresh) || strings.Contains(out, probeSignInAccess) {
		t.Fatalf("list prints a token: %s", out)
	}
	out, _, err = runCLI(t, nil, "model-signins", "remove", "chatgpt")
	if err != nil || !strings.Contains(out, "removed the chatgpt sign-in (revocation: "+modelsignin.RevokeDone+")") {
		t.Fatalf("remove = %q (%v)", out, err)
	}
	if out, _, _ = runCLI(t, nil, "model-signins", "list"); !strings.Contains(out, "no model sign-ins kept") {
		t.Fatalf("after remove: %s", out)
	}
	if _, _, err = runCLI(t, nil, "model-signins", "remove", "chatgpt"); err == nil || !strings.Contains(err.Error(), "no chatgpt sign-in") {
		t.Fatalf("a second remove: %v", err)
	}
}

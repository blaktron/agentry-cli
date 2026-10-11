package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/copilottest"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// copilotModelsFixture is a list's params and a docker whose list container
// is srv, a fake Copilot CLI.
func copilotModelsFixture(srv *copilottest.Server, life time.Duration, renewals *int) (CopilotModelsParams, *dockerx.Fake) {
	pol := policy.Default()
	h, _ := pol.Images.HarnessImageFor(adapter.CopilotImageAdapterName)
	f := &dockerx.Fake{
		ExistingImages: map[string]bool{h.Ref: true},
		Foreground: map[string]*dockerx.FakeForeground{
			"-models": {Handler: func(_ context.Context, _ dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
				// What Copilot CLI might say of the token it was handed.
				_, _ = io.WriteString(stderr, "debug: token "+copilotToken+"\n")
				if err := srv.Serve(stdin, stdout); err != nil {
					return 1, err
				}
				return 0, nil
			}},
		},
	}
	return CopilotModelsParams{
		Policy: pol, CopilotPlan: policy.CopilotPlanBusiness, RunnerImage: "agentry-runner:test",
		SignIn: &ModelSignInGrant{Kind: policy.KeyKindCopilotSignIn,
			Token: ConnectionToken{AccessToken: copilotToken, ExpiresAt: time.Now().Add(life)}},
		RenewSignIn: func(context.Context) (ConnectionToken, error) {
			*renewals++
			return ConnectionToken{AccessToken: copilotRenewed, ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
		},
	}, f
}

// A Copilot plan's models are listed by Copilot CLI in a run's container:
// the Copilot image hardened as an agent is, on an internal network of its
// own whose one route out is a run's proxy allowing GitHub's API and the
// plan's Copilot host alone, nothing mounted, no token in any docker call;
// the token is handed through the callback; everything is removed after.
func TestACopilotModelListRunsInARunsContainer(t *testing.T) {
	srv := &copilottest.Server{Models: []map[string]any{
		{"id": "gpt-5.5", "name": "GPT-5.5", "billing": map[string]any{"multiplier": 1}},
		{"id": "claude-opus-5.5", "policy": map[string]any{"state": "disabled"}},
	}}
	renewals := 0
	p, f := copilotModelsFixture(srv, 8*time.Hour, &renewals)
	var stderr strings.Builder
	p.Stderr = &stderr
	got, err := ListCopilotModels(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	if out := stderr.String(); !strings.Contains(out, "debug: token") || strings.Contains(out, copilotToken) {
		t.Fatalf("Copilot CLI's stderr is not passed on masked: %q", out)
	}
	if len(got) != 1 || got[0].ID != "gpt-5.5" || got[0].Multiplier != 1 {
		t.Fatalf("models %+v", got)
	}
	ops := f.Ops()
	var net string
	for _, op := range ops {
		if strings.HasPrefix(op, "NetworkCreate:") {
			if !strings.HasSuffix(op, "(internal=true)") {
				t.Fatalf("the list's network is not internal: %s", op)
			}
			net = strings.TrimSuffix(strings.TrimPrefix(op, "NetworkCreate:"), "(internal=true)")
		}
	}
	proxy, ok := f.FindRun("-proxy")
	if !ok || proxy.Image != "agentry-runner:test" || proxy.Network != net {
		t.Fatalf("proxy %+v", proxy)
	}
	allow, _ := policy.CopilotProxyAllow(policy.CopilotPlanBusiness)
	if i := slices.Index(proxy.Argv, "--allow"); i < 0 || proxy.Argv[i+1] != strings.Join(allow, ",") {
		t.Fatalf("proxy argv %v, want --allow %v", proxy.Argv, allow)
	}
	if !slices.Contains(ops, "NetworkConnect:bridge<-"+proxy.Name) {
		t.Fatalf("the proxy has no leg out: %v", ops)
	}
	c, ok := f.FindRun("-models")
	if !ok {
		t.Fatal("no list container ran")
	}
	h, _ := p.Policy.Images.HarnessImageFor(adapter.CopilotImageAdapterName)
	if c.Image != h.Ref || c.Network != net || len(c.Mounts) != 0 || !c.ReadOnly || !c.Interactive || !c.AutoRemove ||
		!slices.Equal(c.CapDrop, []string{"ALL"}) || !c.NoNewPrivileges || c.Memory == "" || c.PidsLimit == 0 {
		t.Fatalf("list container %+v", c)
	}
	if c.Env["HTTPS_PROXY"] == "" || len(c.Tmpfs) == 0 {
		t.Fatalf("list container env %v tmpfs %v", c.Env, c.Tmpfs)
	}
	for _, call := range f.Calls {
		if s := fmt.Sprintf("%+v", call); strings.Contains(s, copilotToken) || strings.Contains(s, "ghu_") {
			t.Fatalf("a docker call carries the token: %s", s)
		}
	}
	_, _, toks, _, _ := srv.Snapshot()
	if len(toks) != 1 || toks[0]["accessToken"] != copilotToken || renewals != 0 {
		t.Fatalf("tokens %v, renewals %d", toks, renewals)
	}
	if !slices.Contains(ops, "NetworkRemove:"+net) || !slices.Contains(ops, "RemoveForce:"+proxy.Name) {
		t.Fatalf("the list left its network or proxy: %v", ops)
	}
}

// The token is renewed as a run's is: one with an hour or less left, which
// Copilot CLI would refuse, is renewed before it is handed over.
func TestACopilotModelListRenewsAShortLivedToken(t *testing.T) {
	srv := &copilottest.Server{Models: []map[string]any{{"id": "gpt-5.5"}}}
	renewals := 0
	p, f := copilotModelsFixture(srv, 30*time.Minute, &renewals)
	if _, err := ListCopilotModels(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	_, _, toks, _, _ := srv.Snapshot()
	if renewals != 1 || len(toks) != 1 || toks[0]["accessToken"] != copilotRenewed {
		t.Fatalf("tokens %v, renewals %d", toks, renewals)
	}
}

// A list is refused before docker without a sign-in, for a plan that is not
// one, or without the Copilot image; an organization's policy is said as a
// run's is, and the token is masked out of every error.
func TestACopilotModelListRefusesWhatARunWould(t *testing.T) {
	renewals := 0
	p, f := copilotModelsFixture(&copilottest.Server{}, 8*time.Hour, &renewals)
	bad := p
	bad.SignIn = nil
	if _, err := ListCopilotModels(context.Background(), f, bad); err == nil {
		t.Fatal("listed with no sign-in")
	}
	bad = p
	bad.CopilotPlan = "free"
	if _, err := ListCopilotModels(context.Background(), f, bad); err == nil {
		t.Fatal("listed for a plan that is not one")
	}
	f.ExistingImages = nil
	if _, err := ListCopilotModels(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "agentry setup builds it") {
		t.Fatalf("no image: %v", err)
	}
	if slices.ContainsFunc(f.Ops(), func(op string) bool { return strings.HasPrefix(op, "Run") }) {
		t.Fatalf("a refused list started a container: %v", f.Ops())
	}

	p, f = copilotModelsFixture(&copilottest.Server{CreateError: "Error: Access denied by policy settings for " + copilotToken}, 8*time.Hour, &renewals)
	_, err := ListCopilotModels(context.Background(), f, p)
	var refused *adapter.OrgPolicyRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("an organization's policy: %v", err)
	}
	if strings.Contains(err.Error(), copilotToken) {
		t.Fatalf("the error carries the token: %v", err)
	}
}

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
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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
				// And far more than the caller keeps of it.
				_, _ = io.WriteString(stderr, strings.Repeat("x", 200<<10))
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
		t.Fatalf("Copilot CLI's stderr is not passed on masked: %.200q", out)
	}
	// Copilot CLI's stderr is kept to its first 64 KiB.
	if n := stderr.Len(); n < 60<<10 || n > 66<<10 {
		t.Fatalf("Copilot CLI's stderr passed on is %d bytes, want about 64 KiB", n)
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
	if proxy.Runtime != "" || c.Runtime != "" || len(c.ExtraHosts) != 0 {
		t.Fatalf("a list under no runtime ran under %q / %q with hosts %v", proxy.Runtime, c.Runtime, c.ExtraHosts)
	}

	// Under the policy's runtime, as a run is: the list container runs under
	// it and, since a sandboxed container does not reach docker's DNS, is
	// given the proxy's address.
	p, f = copilotModelsFixture(srv, 8*time.Hour, &renewals)
	p.Policy.Docker.Runtime = "runsc"
	p.Policy.Docker.Resolvers = []string{"9.9.9.9"}
	p.Policy.Docker.Limits = policy.Limits{Memory: "512m", CPUs: 2, Pids: 100}
	f.IPs = map[string]string{}
	if _, err := ListCopilotModels(context.Background(), f, p); err != nil {
		t.Fatal(err)
	}
	sandboxedProxy, _ := f.FindRun("-proxy")
	sandboxed, ok := f.FindRun("-models")
	if !ok || sandboxed.Runtime != "runsc" || sandboxedProxy.Runtime != "runsc" {
		t.Fatalf("the list under a runtime: container %q, proxy %q", sandboxed.Runtime, sandboxedProxy.Runtime)
	}
	if sandboxed.Memory != "512m" || sandboxed.PidsLimit != 100 {
		t.Fatalf("the list container does not take the policy's limits: memory %q, pids %d", sandboxed.Memory, sandboxed.PidsLimit)
	}
	if len(sandboxed.ExtraHosts) != 1 || !strings.HasPrefix(sandboxed.ExtraHosts[0], spec.ProxyAlias+":172.30.0.") {
		t.Fatalf("the sandboxed list is not pinned to the proxy: %v", sandboxed.ExtraHosts)
	}
}

// The token is renewed as a run's is: one with an hour or less left, which
// Copilot CLI would refuse, is renewed before it is handed over, and one
// with more than the hour and five minutes it needs is not. The renewed
// token is masked out of what Copilot CLI says; a renewal that fails, or
// answers no token or one that is no access token, fails the list, says why
// without the token, and hands Copilot CLI no token at all.
func TestACopilotModelListRenewsAShortLivedToken(t *testing.T) {
	for _, c := range []struct {
		life  time.Duration
		renew bool
	}{
		{30 * time.Minute, true},
		{62 * time.Minute, true},
		{68 * time.Minute, false},
		{90 * time.Minute, false},
		{8 * time.Hour, false},
	} {
		srv := &copilottest.Server{Models: []map[string]any{{"id": "gpt-5.5"}}}
		renewals := 0
		p, f := copilotModelsFixture(srv, c.life, &renewals)
		inner := f.Foreground["-models"].Handler
		f.Foreground["-models"].Handler = func(ctx context.Context, r dockerx.RunSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			_, _ = io.WriteString(stderr, "debug: renewed "+copilotRenewed+"\n")
			return inner(ctx, r, stdin, stdout, stderr)
		}
		var stderr strings.Builder
		p.Stderr = &stderr
		if _, err := ListCopilotModels(context.Background(), f, p); err != nil {
			t.Fatalf("%v left: %v", c.life, err)
		}
		_, _, toks, _, _ := srv.Snapshot()
		want, wantRenewals := copilotToken, 0
		if c.renew {
			want, wantRenewals = copilotRenewed, 1
		}
		if renewals != wantRenewals || len(toks) != 1 || toks[0]["accessToken"] != want {
			t.Fatalf("%v left: tokens %v, renewals %d, want %d with %s", c.life, toks, renewals, wantRenewals, want)
		}
		if c.renew && (!strings.Contains(stderr.String(), "debug: renewed") || strings.Contains(stderr.String(), copilotRenewed)) {
			t.Fatalf("the renewed token is not masked out of Copilot CLI's stderr: %.200q", stderr.String())
		}
	}

	for _, c := range []struct {
		name  string
		token string
		err   error
		say   string
	}{
		{"a refused renewal", "", errors.New("the vendor refused " + copilotToken), "the vendor refused"},
		{"no token", "", nil, "the vendor sent no access token"},
		{"a token of no shape", "not an access token\n", nil, "the vendor's new access token is not in the shape an access token takes"},
	} {
		srv := &copilottest.Server{Models: []map[string]any{{"id": "gpt-5.5"}}}
		renewals := 0
		p, f := copilotModelsFixture(srv, 30*time.Minute, &renewals)
		p.RenewSignIn = func(context.Context) (ConnectionToken, error) {
			return ConnectionToken{AccessToken: c.token, ExpiresAt: time.Now().Add(8 * time.Hour)}, c.err
		}
		var stderr strings.Builder
		p.Stderr = &stderr
		got, err := ListCopilotModels(context.Background(), f, p)
		if err == nil || got != nil {
			t.Fatalf("%s: listed %v", c.name, got)
		}
		_, _, toks, _, _ := srv.Snapshot()
		if len(toks) != 1 || toks[0]["accessToken"] != nil || toks[0]["error"] == nil {
			t.Fatalf("%s: Copilot CLI was handed %v", c.name, toks)
		}
		note := "note: could not renew the model sign-in's access token: "
		out := stderr.String()
		if !strings.Contains(out, note) || !strings.Contains(out, c.say) || strings.Contains(out, copilotToken) || strings.Contains(out, "ghu_") {
			t.Fatalf("%s: stderr %.300q", c.name, out)
		}
		if strings.Contains(err.Error(), copilotToken) {
			t.Fatalf("%s: the error carries the token: %v", c.name, err)
		}
	}
}

// A list is refused before docker without a sign-in, for a sign-in of
// another kind or with no token, for a plan that is not one, without the
// runner image or without the Copilot image; an organization's policy is
// said as a run's is, and the token is masked out of every error.
func TestACopilotModelListRefusesWhatARunWould(t *testing.T) {
	renewals := 0
	p, f := copilotModelsFixture(&copilottest.Server{}, 8*time.Hour, &renewals)
	refuse := func(name string, edit func(*CopilotModelsParams), say string) {
		t.Helper()
		bad := p
		edit(&bad)
		if _, err := ListCopilotModels(context.Background(), f, bad); err == nil || !strings.Contains(err.Error(), say) {
			t.Fatalf("%s: %v, want a refusal saying %q", name, err, say)
		}
	}
	noSignIn := "no Copilot sign-in to list the models of"
	refuse("no sign-in", func(b *CopilotModelsParams) { b.SignIn = nil }, noSignIn)
	refuse("a sign-in of another kind", func(b *CopilotModelsParams) {
		b.SignIn = &ModelSignInGrant{Kind: policy.KeyKindClaudeSignIn, Token: ConnectionToken{AccessToken: copilotToken}}
	}, noSignIn)
	refuse("a sign-in with no token", func(b *CopilotModelsParams) {
		b.SignIn = &ModelSignInGrant{Kind: policy.KeyKindCopilotSignIn}
	}, noSignIn)
	refuse("no policy", func(b *CopilotModelsParams) { b.Policy = nil }, "no policy or runner image")
	refuse("no runner image", func(b *CopilotModelsParams) { b.RunnerImage = "" }, "no policy or runner image")
	refuse("a plan that is not one", func(b *CopilotModelsParams) { b.CopilotPlan = "free" }, `no Copilot plan "free"`)
	if slices.ContainsFunc(f.Ops(), func(op string) bool { return strings.HasPrefix(op, "Run") || strings.HasPrefix(op, "Network") }) {
		t.Fatalf("a refused list touched the daemon: %v", f.Ops())
	}
	// The image is pinned by the policy or, failing that, by the build.
	pinned := policy.DefaultHarnessImages[adapter.CopilotImageAdapterName]
	delete(policy.DefaultHarnessImages, adapter.CopilotImageAdapterName)
	refuse("a policy that pins no Copilot image", func(b *CopilotModelsParams) {
		b.Policy = policy.Default()
		b.Policy.Images.HarnessImages = nil
	}, "the policy pins no Copilot image")
	policy.DefaultHarnessImages[adapter.CopilotImageAdapterName] = pinned
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
	if strings.Contains(err.Error(), copilotToken) || strings.Contains(refused.Text, copilotToken) || !strings.Contains(refused.Text, "Access denied by policy settings") {
		t.Fatalf("the error carries the token: %v", err)
	}

	// Any other failure of the list is worded as the list's and masked too.
	p, f = copilotModelsFixture(&copilottest.Server{CreateError: "Error: the backend failed for " + copilotToken}, 8*time.Hour, &renewals)
	_, err = ListCopilotModels(context.Background(), f, p)
	if err == nil || errors.As(err, &refused) || !strings.HasPrefix(err.Error(), "copilot models: ") || !strings.Contains(err.Error(), "the backend failed") ||
		strings.Contains(err.Error(), copilotToken) {
		t.Fatalf("a failed list: %v", err)
	}
}

// The Copilot sign-in a model list is given is the Copilot one in the
// person's store: with none kept, the list is refused as a run would be, for
// want of a sign-in, and says it is the Copilot one it wants.
func TestACopilotModelListSignInComesFromThePersonsStore(t *testing.T) {
	grant, renew, err := CopilotSignIn(context.Background(), modelsignin.Open(t.TempDir(), nil))
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy || !errors.Is(err, modelsignin.ErrNoSignIn) ||
		!strings.Contains(err.Error(), policy.SignInSentence(policy.KeyKindCopilotSignIn)) {
		t.Fatalf("no sign-in kept: %v", err)
	}
	if r := receipt.ReasonOf(err); r == nil || r.Code != receipt.ReasonSignInMissing || grant != nil || renew != nil {
		t.Fatalf("no sign-in kept: reason %v, grant %v, renewal %v", r, grant, renew != nil)
	}
}

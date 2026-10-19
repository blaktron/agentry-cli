package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

// testReviewKey is the person's TypeSafe key a review run carries.
const testReviewKey = "jev-person-review-key-0123456789abcdef"

// reviewFixture compiles a plan with the review grant, as localrun does for
// a built-in granted it, and a run of it with the person's key.
func reviewFixture(t *testing.T) (Params, *dockerx.Fake) {
	t.Helper()
	m, err := manifest.Parse([]byte(webManifest))
	if err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Review it.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	planDir := filepath.Join(installDir, "plan")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Digest: "sha256:4e71e", Source: "builtin", Review: true,
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test",
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, err := adapter.Get(adapter.DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("AGENTRY_TOKEN", testRegToken)
	t.Setenv("SECUREAGENTRY_TOKEN", testRegToken)
	inputs := t.TempDir()
	if err := os.WriteFile(filepath.Join(inputs, "agent.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Task: strings.NewReader("review this"), Adapter: ad, Policy: pol, InputPaths: []string{filepath.Join(inputs, "agent.json")},
		Budgets:     spec.Budgets{TimeoutSeconds: 30, MaxTurns: 10},
		Attestation: receipt.Attestation{State: "builtin"}, Stderr: os.Stderr, RunID: "4e710000",
		Review: &ReviewRun{JevKey: testReviewKey},
	}, &dockerx.Fake{}
}

// TestAReviewsKeyReachesTheToolsContainerAlone (mirroring
// TestADraftsKeysReachTheToolsContainerAlone): no container's environment or
// command line holds the person's TypeSafe key, the agent's names nothing of
// it, the key is in the run's gateway secrets file for the tools server
// alone and gone at teardown, and the receipts are masked of it.
func TestAReviewsKeyReachesTheToolsContainerAlone(t *testing.T) {
	p, f := reviewFixture(t)
	happyFake(f)
	f.LogsByName = map[string]string{"agentry-" + p.RunID + "-gateway": "tools server env AGENTRY_REVIEW_JEV_KEY=" + testReviewKey + "\n"}
	secretsPath := filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway", "secrets.env")
	var secretsFile string
	f.OnForeground = func(r dockerx.RunSpec) {
		if strings.HasSuffix(r.Name, "-agent") {
			b, _ := os.ReadFile(secretsPath)
			secretsFile = string(b)
		}
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{testReviewKey, testRegToken}
	sawAgent := false
	for _, c := range f.Calls {
		r := c.Run
		if r.Name == "" {
			continue
		}
		if strings.HasSuffix(r.Name, "-agent") {
			sawAgent = true
			for name := range r.Env {
				if strings.HasPrefix(name, "AGENTRY_") || strings.Contains(name, "JEV") || strings.Contains(name, "TYPESAFE") {
					t.Errorf("the agent container's environment names %s", name)
				}
			}
		}
		for name, v := range r.Env {
			for _, k := range keys {
				if strings.Contains(v, k) {
					t.Errorf("%s carries a key in %s", r.Name, name)
				}
			}
		}
		for _, a := range r.Argv {
			for _, k := range keys {
				if strings.Contains(a, k) {
					t.Errorf("%s carries a key on its command line", r.Name)
				}
			}
		}
	}
	if !sawAgent {
		t.Fatal("no agent container ran")
	}
	if !strings.Contains(secretsFile, compile.SecretKey(toolsrv.EnvReviewJevKey)+"="+testReviewKey) || strings.Contains(secretsFile, testRegToken) {
		t.Fatalf("the gateway secrets file during the run: %q", secretsFile)
	}
	if _, err := os.Stat(secretsPath); !os.IsNotExist(err) {
		t.Fatal("secrets.env survived the run")
	}
	// The catalog gives the key to the tools server, and to no other entry.
	catalog, _ := os.ReadFile(filepath.Join(p.PlanDir, "gateway", "catalog.yaml"))
	if n := strings.Count(string(catalog), "env: "+toolsrv.EnvReviewJevKey); n != 1 {
		t.Fatalf("the catalog names the review key %d times:\n%s", n, catalog)
	}
	b, _ := json.Marshal(env)
	gw, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "gateway.log"))
	if strings.Contains(string(b), testReviewKey) || strings.Contains(string(gw), testReviewKey) || len(gw) == 0 {
		t.Fatalf("a receipt carries the key: envelope %v, gateway.log %q", strings.Contains(string(b), testReviewKey), gw)
	}
}

// TestAReviewPlanRunsOnlyWithItsKey: without the person's key a run is
// refused before any container; a plan without the grant refuses a key; a
// session never runs it.
func TestAReviewPlanRunsOnlyWithItsKey(t *testing.T) {
	for name, r := range map[string]*ReviewRun{"none": nil, "empty": {}} {
		p, f := reviewFixture(t)
		p.Review = r
		_, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Code != ExitPolicy || !errors.Is(err, ErrNoReviewKey) {
			t.Fatalf("%s: %v", name, err)
		}
		if len(f.Calls) != 0 {
			t.Fatalf("%s: docker was called: %+v", name, f.Calls)
		}
	}
	p, f := fixture(t)
	p.Review = &ReviewRun{JevKey: testReviewKey}
	if _, err := Run(context.Background(), f, p); err == nil {
		t.Fatal("a plan without the review grant took a key")
	}
	// A session refuses the grant whether the plan carries it or only the
	// run brings a key.
	for _, viaPlan := range []bool{true, false} {
		p, f := sessionFixture(t)
		if viaPlan {
			p.Plan.Capabilities.ReviewTools = true
		} else {
			p.Review = &ReviewRun{JevKey: testReviewKey}
		}
		if _, err := StartSession(context.Background(), f, p); err == nil || !strings.Contains(err.Error(), "review grant") {
			t.Fatalf("a session (via plan %v): %v", viaPlan, err)
		}
		// The sweep of dead runs' leftovers goes first; nothing of this
		// session's starts.
		for _, c := range f.Calls {
			if c.Run.Name != "" {
				t.Fatalf("a session (via plan %v) started %s", viaPlan, c.Run.Name)
			}
		}
	}
}

// TestTheReviewLedgerIsStampedAndCounted: the run's tools spec names the
// ledger and no key, the allowlist grants ai_review, the docker filter's
// relays may reach TypeSafe's API alone, and what the tools server wrote is
// counted into the receipt (a line that does not decode is never a pass).
func TestTheReviewLedgerIsStampedAndCounted(t *testing.T) {
	p, f := reviewFixture(t)
	happyFake(f)
	p.KeepRunDir = true
	ledger := strings.Join([]string{
		`{"tool":"read_input","path":"pkg/AGENT.md","bytes":900,"verdict":"pass","model":"jev-1.13.0","questionSet":"sha256:q","requests":1,"inputTokens":300}`,
		`{"tool":"read_input","path":"pkg/run.sh","bytes":90,"verdict":"withheld","score":3,"flagged":["I1"],"requests":2,"inputTokens":40}`,
		`{"tool":"read_input","path":"pkg/huge.md","bytes":1048577,"verdict":"not_scanned","failure":"too-large"}`,
		`{"tool":"read_input","path":"pkg/logo.png","refused":"binary file"}`,
		`{"tool":"ai_review","files":3,"notRead":1,"rubricHash":"sha256:r","findings":2,"verdict":"withheld","score":3,"model":"jev-1.13.0","requests":10,"inputTokens":5000}`,
		`{"tool":"ai_review","files":3,"verdict":"not_scanned","failure":"timeout","requests":2,"inputTokens":100}`,
		`{"verdict":"pass"`,
	}, "\n") + "\n"
	f.OnForeground = func(r dockerx.RunSpec) {
		if strings.HasSuffix(r.Name, "-agent") {
			_ = os.WriteFile(filepath.Join(p.OutDir, "receipts", ReviewLedgerFile), []byte(ledger), 0o644)
		}
	}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	gw := filepath.Join(p.RunRoot, "runs", p.RunID, "cfg", "gateway")
	b, err := os.ReadFile(filepath.Join(gw, "toolspec.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ts toolsrv.Spec
	if err := json.Unmarshal(b, &ts); err != nil {
		t.Fatal(err)
	}
	if !ts.Review.Enabled || ts.Review.Ledger != "/out/receipts/"+ReviewLedgerFile || strings.Contains(string(b), testReviewKey) {
		t.Fatalf("review %+v", ts.Review)
	}
	if !slices.Equal(ts.GrantedTools(), []string{toolsrv.ToolListInputs, toolsrv.ToolReadInput, toolsrv.ToolAIReview, toolsrv.ToolScratchList, toolsrv.ToolScratchRead, toolsrv.ToolScratchWrite, toolsrv.ToolScratchSearch, toolsrv.ToolGrep, toolsrv.ToolGlob, toolsrv.ToolWriteReport}) {
		t.Fatalf("granted %v", ts.GrantedTools())
	}
	tools, _ := os.ReadFile(filepath.Join(gw, "tools.yaml"))
	if !strings.Contains(string(tools), toolsrv.ToolAIReview) {
		t.Fatalf("tools.yaml lacks ai_review:\n%s", tools)
	}
	if hosts := relayHosts(p.Plan); !slices.Equal(hosts, []string{toolsrv.ReviewHost + ":443"}) {
		t.Fatalf("relay hosts %v", hosts)
	}

	r := env.Review
	if r == nil || r.Read != 3 || r.Passed != 1 || r.Withheld != 1 || r.NotScanned != 1 || r.Refused != 2 || r.Reviews != 1 || r.Failed != 1 ||
		r.Findings != 2 || r.ScanRequests != 15 || r.ScanInputTokens != 5440 || r.Model != "jev-1.13.0" || r.RubricHash != "sha256:r" || r.QuestionSet != "sha256:q" {
		t.Fatalf("%+v", r)
	}
	line := env.CapabilitiesEnforced["review.aiReview"]
	if !strings.Contains(line, "3 read, 1 passed, 1 withheld as flagged, 1 withheld as not scanned; 2 refused") ||
		!strings.Contains(line, "1 rubric review(s) answered with 2 finding(s)") || !strings.Contains(line, toolsrv.ReviewHost+":443 alone") {
		t.Fatalf("%q", line)
	}
	if filterSupported != strings.Contains(line, "the run's docker filter admits that proxy toward no other host") {
		t.Fatalf("the network clause on this platform: %q", line)
	}
	if !strings.Contains(env.CapabilitiesEnforced["review"], "never the package's text") || !strings.Contains(env.CapabilitiesEnforced["network"], toolsrv.ReviewHost) {
		t.Fatalf("%v", env.CapabilitiesEnforced)
	}
}

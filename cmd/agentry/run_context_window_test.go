package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// stubWindowProbe answers the run's context-window probe with found, and
// counts the calls and records the key it was given.
func stubWindowProbe(t *testing.T, found setup.ContextWindow, err error) (calls *int, key *string) {
	t.Helper()
	n, k := 0, ""
	prev := probeContextWindow
	probeContextWindow = func(_ context.Context, _, _, apiKey string) (setup.ContextWindow, error) {
		n++
		k = apiKey
		return found, err
	}
	t.Cleanup(func() { probeContextWindow = prev })
	return &n, &k
}

func ownEndpointPolicy(window int64) *policy.Policy {
	return &policy.Policy{Model: policy.Model{Provider: "custom", Name: "local-llm", APIKeyEnv: "LOCAL_KEY",
		BaseURL: "http://127.0.0.1:1234/v1", ContextWindow: window, ProxyAllow: []string{"127.0.0.1:1234"}}}
}

// TestRunContextWindow: a model of the person's own with no window in the
// policy is asked for one, which is used for this run and said; one that
// reports none goes on and says so; any other model, or a window already in
// the policy, asks nothing.
func TestRunContextWindow(t *testing.T) {
	isolateEnv(t)
	t.Setenv("LOCAL_KEY", "sk-local")

	t.Run("found", func(t *testing.T) {
		calls, key := stubWindowProbe(t, setup.ContextWindow{Tokens: 131072, Source: "http://127.0.0.1:1234/v1/models"}, nil)
		pol := ownEndpointPolicy(0)
		var out bytes.Buffer
		source := runContextWindow(context.Background(), &out, pol)
		if *calls != 1 || *key != "sk-local" {
			t.Fatalf("probe calls = %d, key %q", *calls, *key)
		}
		if pol.Model.ContextWindow != 131072 || source != "http://127.0.0.1:1234/v1/models" {
			t.Errorf("window %d from %q", pol.Model.ContextWindow, source)
		}
		if want := "context window of local-llm: 131072 tokens, as http://127.0.0.1:1234/v1/models reports it (for this run; `agentry setup` records it in the policy)\n"; out.String() != want {
			t.Errorf("said %q, want %q", out.String(), want)
		}
	})
	t.Run("not found", func(t *testing.T) {
		calls, _ := stubWindowProbe(t, setup.ContextWindow{}, nil)
		pol := ownEndpointPolicy(0)
		var out bytes.Buffer
		if source := runContextWindow(context.Background(), &out, pol); source != "" || pol.Model.ContextWindow != 0 || *calls != 1 {
			t.Fatalf("source %q, window %d, calls %d", source, pol.Model.ContextWindow, *calls)
		}
		if !strings.Contains(out.String(), "context window of local-llm: unknown, the endpoint did not report one") {
			t.Errorf("said %q", out.String())
		}
	})
	t.Run("out of bounds", func(t *testing.T) {
		stubWindowProbe(t, setup.ContextWindow{Tokens: 1 << 62, Source: "http://127.0.0.1:1234/props"}, nil)
		pol := ownEndpointPolicy(0)
		var out bytes.Buffer
		if source := runContextWindow(context.Background(), &out, pol); source != "" || pol.Model.ContextWindow != 0 {
			t.Fatalf("source %q, window %d", source, pol.Model.ContextWindow)
		}
		if !strings.Contains(out.String(), "not taken: http://127.0.0.1:1234/props reports") || !strings.Contains(out.String(), ": unknown,") {
			t.Errorf("said %q", out.String())
		}
	})
	t.Run("not an endpoint of the person's own", func(t *testing.T) {
		calls, _ := stubWindowProbe(t, setup.ContextWindow{Tokens: 131072, Source: "x"}, nil)
		pol := &policy.Policy{Model: policy.Model{Provider: "anthropic", Name: "claude-sonnet-5-5", APIKeyEnv: "ANTHROPIC_API_KEY"}}
		var out bytes.Buffer
		if source := runContextWindow(context.Background(), &out, pol); source != "" || *calls != 0 || out.Len() != 0 || pol.Model.ContextWindow != 0 {
			t.Errorf("source %q, calls %d, said %q, window %d", source, *calls, out.String(), pol.Model.ContextWindow)
		}
	})
	t.Run("a window in the policy", func(t *testing.T) {
		calls, _ := stubWindowProbe(t, setup.ContextWindow{Tokens: 131072, Source: "x"}, nil)
		pol := ownEndpointPolicy(40960)
		var out bytes.Buffer
		if source := runContextWindow(context.Background(), &out, pol); source != "" || *calls != 0 || out.Len() != 0 || pol.Model.ContextWindow != 40960 {
			t.Errorf("source %q, calls %d, said %q, window %d", source, *calls, out.String(), pol.Model.ContextWindow)
		}
	})
}

// fakeKeychain is a keychain holding fixed accounts.
type fakeKeychain map[string]string

func (k fakeKeychain) Get(account string) (string, error) {
	if v, ok := k[account]; ok {
		return v, nil
	}
	return "", modelkey.ErrNotStored
}
func (k fakeKeychain) Set(account, value string) error { k[account] = value; return nil }
func (k fakeKeychain) Delete(account string) error     { delete(k, account); return nil }

// TestRunContextWindowKeyFromKeychain: with the key only in the keychain the
// policy names, the probe is given it.
func TestRunContextWindowKeyFromKeychain(t *testing.T) {
	isolateEnv(t)
	t.Setenv("LOCAL_KEY", "")
	restore := modelkey.UseKeychain(fakeKeychain{modelkey.Account("custom"): "sk-kept"})
	t.Cleanup(restore)
	_, key := stubWindowProbe(t, setup.ContextWindow{Tokens: 32768, Source: "http://127.0.0.1:1234/props"}, nil)
	pol := ownEndpointPolicy(0)
	pol.Model.APIKeySource = modelkey.SourceKeyring
	runContextWindow(context.Background(), &bytes.Buffer{}, pol)
	if *key != "sk-kept" {
		t.Errorf("the probe was given %q", *key)
	}
}

// TestRunProbesTheWindowForThisRunOnly: a dry run on a model of the person's
// own whose policy has no window asks the endpoint, compiles the window it
// reports into the plan with its source, and leaves the policy file as it
// was; when the endpoint reports another window, the plan is compiled again.
func TestRunProbesTheWindowForThisRunOnly(t *testing.T) {
	isolateEnv(t)
	_, policyPath := isolateRunnerHome(t)
	var window atomic.Int64
	window.Store(65536)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "local-llm", "max_model_len": window.Load()}}})
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	pol, _, err := policy.EnsureDefault(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	pol.Model = policy.Model{Provider: "custom", Name: "local-llm", APIKeyEnv: "LOCAL_KEY", BaseURL: srv.URL + "/v1", ProxyAllow: []string{host}}
	if err := policy.Save(policyPath, pol); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}

	args := []string{"--json", "run", "--local", repoPath(t, "testdata/runner/pure-text"), "--allow-unsigned", "--dry-run", "--policy", policyPath}
	plan := func() (map[string]any, string) {
		t.Helper()
		stdout, stderr, err := runCLI(t, strings.NewReader(""), args...)
		if err != nil {
			t.Fatalf("dry run: %v (stderr %q)", err, stderr)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(stdout), &p); err != nil {
			t.Fatalf("stdout is not the plan: %v", err)
		}
		return p, stderr
	}
	p, stderr := plan()
	source := srv.URL + "/v1/models"
	if want := "context window of local-llm: 65536 tokens, as " + source + " reports it"; !strings.Contains(stderr, want) {
		t.Errorf("stderr lacks %q:\n%s", want, stderr)
	}
	harness, _ := p["harness"].(map[string]any)
	if got, _ := json.Marshal(harness["contextWindow"]); string(got) != `{"source":"`+source+`","tokens":65536}` {
		t.Errorf("plan harness.contextWindow = %s", got)
	}
	var agentYAML []byte
	_ = filepath.WalkDir(planDirOf(t, stderr), func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "agent.yaml" {
			agentYAML, _ = os.ReadFile(path) //nolint:gosec // the test's own plan dir
		}
		return nil
	})
	if !strings.Contains(string(agentYAML), "context_size: 65536") {
		t.Errorf("the harness config lacks the window:\n%s", agentYAML)
	}
	after, _ := os.ReadFile(policyPath)
	if !bytes.Equal(before, after) {
		t.Errorf("the policy file changed:\n%s", after)
	}

	window.Store(32768)
	p, _ = plan()
	harness, _ = p["harness"].(map[string]any)
	if got, _ := json.Marshal(harness["contextWindow"]); string(got) != `{"source":"`+source+`","tokens":32768}` {
		t.Errorf("after the endpoint's window changed, plan harness.contextWindow = %s", got)
	}
}

// planDirOf reads the plan directory a dry run names on stderr.
func planDirOf(t *testing.T, stderr string) string {
	t.Helper()
	for line := range strings.SplitSeq(stderr, "\n") {
		if dir, ok := strings.CutPrefix(line, "plan dir: "); ok {
			return strings.TrimSpace(dir)
		}
	}
	t.Fatalf("no plan dir line in %q", stderr)
	return ""
}

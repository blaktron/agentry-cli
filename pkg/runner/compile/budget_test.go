package compile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// TestThePlanStampsItsHarnessResultBudget: the tools spec carries the run's
// adapter's result budget, and the System Prompt states the same size, so
// the pages the tools serve and what the agent is told agree for every
// harness.
func TestThePlanStampsItsHarnessResultBudget(t *testing.T) {
	for _, tc := range []struct {
		adapter string
		say     string
		shape   func(*policy.Policy)
	}{
		{adapter: adapter.DefaultName, say: "A tool result holds at most 48 KiB and 1,900 lines,"},
		{adapter: adapter.DefaultName, say: "A tool result holds at most 16 KiB and 1,900 lines,", shape: func(pol *policy.Policy) {
			pol.Model = policy.Model{Provider: "custom", Name: "qwen3.8-flash", APIKeyEnv: "CUSTOM_API_KEY", BaseURL: "http://127.0.0.1:1234/v1", ContextWindow: 32768, ProxyAllow: []string{"127.0.0.1:1234"}}
		}},
		{adapter: adapter.DefaultName, say: "A tool result holds at most 32 KiB and 1,900 lines,", shape: func(pol *policy.Policy) {
			pol.Model = policy.Model{Provider: "custom", Name: "qwen3.8-flash", APIKeyEnv: "CUSTOM_API_KEY", BaseURL: "http://127.0.0.1:1234/v1", ProxyAllow: []string{"127.0.0.1:1234"}}
		}},
		{adapter: adapter.CopilotImageAdapterName, say: "A tool result holds at most 19 KiB and 1,900 lines,", shape: func(pol *policy.Policy) {
			allow, _ := policy.CopilotProxyAllow(policy.CopilotPlanIndividual)
			pol.Model = policy.Model{Provider: policy.ProviderCopilot, Name: "auto", APIKeyKind: policy.KeyKindCopilotSignIn, ProxyAllow: allow}
			pol.Images.HarnessImages = nil
		}},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			m := parseManifest(t, pureTextYAML)
			pol := testPolicy()
			if tc.shape != nil {
				tc.shape(pol)
			}
			dir := t.TempDir()
			if _, err := Compile(Inputs{
				Manifest: m, Digest: "sha256:0123456789abcdef", Source: "local",
				WorkspaceDir: workspaceFor(t, m, nil), Policy: pol, PolicySHA: "fixedpolicysha",
				RunnerImage: "agentry-runner:test", AdapterName: tc.adapter,
			}, dir); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "gateway", "toolspec.json"))
			if err != nil {
				t.Fatal(err)
			}
			var ts toolsrv.Spec
			if err := json.Unmarshal(raw, &ts); err != nil {
				t.Fatal(err)
			}
			want := adapter.PageBudget(tc.adapter, pol.Model)
			if ts.Limits.MaxResultBytes != want.Bytes || ts.Limits.MaxResultLines != want.Lines {
				t.Fatalf("toolspec limits %+v, want the page %+v", ts.Limits, want)
			}
			var said bool
			_ = filepath.WalkDir(filepath.Join(dir, "harness"), func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), tc.say) {
						said = true
					}
				}
				return nil
			})
			if !said {
				t.Fatalf("no harness file states %q", tc.say)
			}
		})
	}
}

// TestTheReceiptSaysWhatSetThePage: the receipt names the harness's limit
// when the page is the harness's, the window when the model's window set
// it, and the missing window when a model server of the person's own has
// none recorded.
func TestTheReceiptSaysWhatSetThePage(t *testing.T) {
	own := policy.Model{Provider: "custom", Name: "m", BaseURL: "http://127.0.0.1:1234/v1"}
	withWindow := own
	withWindow.ContextWindow = 32768
	for _, c := range []struct {
		name  string
		model policy.Model
		say   string
	}{
		{"hosted", policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-5-5"}, "held to 48 KiB and 1,900 lines, the docker-agent harness's measured tool-result limit less a margin,"},
		{"a recorded window", withWindow, "held to 16 KiB and 1,900 lines, the page the model's context window of 32768 tokens sets, under the docker-agent harness's measured tool-result limit,"},
		{"no window recorded", own, "held to 32 KiB and 1,900 lines, the page for a model server of your own whose context window is not recorded, under the docker-agent harness's measured tool-result limit,"},
	} {
		line := toolResultsEnforcement(adapter.DefaultName, c.model, adapter.PageBudget(adapter.DefaultName, c.model), spec.CapSummary{})
		if !strings.Contains(line, c.say) {
			t.Errorf("%s: receipt line %q does not say %q", c.name, line, c.say)
		}
	}
}

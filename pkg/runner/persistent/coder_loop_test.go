package persistent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// TestTheCodingAgentWorksTheChangeLoopOnEveryHarness: the Coding agent's
// change loop is in its instruction and its skills, which are the whole of
// what the model is told whichever harness runs it. A segment planned on
// docker-agent, claude-image and codex-image gives each harness an
// instruction that carries the loop's steps and lists every built-in skill
// the loop names.
func TestTheCodingAgentWorksTheChangeLoopOnEveryHarness(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := coderAgent(t)
	cfg := Config{Enabled: true}
	RecordConsent(BuildRoster(a, cfg, nil), &cfg)
	g, _, done, err := Apply(context.Background(), home, a, cfg, BuildRoster(a, cfg, nil), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer done()

	loop := []string{
		"## Every change goes through the loop",
		"**The project's rules.**",
		"conventions, never",
		"**A branch in a worktree**",
		"**The CI gate**",
		"Merge only on",
		"**Deploy and verify**",
		"**Close with the record**",
		"data/notes/<project>.md",
	}
	skills := []string{"work-change", "ci-gate", "verify-change", "review-change", "open-pull-request", "test-first"}

	for _, tc := range []struct {
		adapter string
		policy  *policy.Policy
		read    func(t *testing.T, planDir string) string
	}{
		{adapter.DefaultName, policy.Default(), agentInstruction},
		{adapter.ClaudeImageAdapterName, claudeSignIn(), harnessInstructionFile},
		{adapter.CodexImageAdapterName, policy.Default(), harnessInstructionFile},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: tc.policy, PolicySHA: "s", Build: "test", Grants: g, Adapter: tc.adapter}.Plan(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if sp.Plan.Harness.Adapter != tc.adapter {
				t.Fatalf("the segment runs on %s", sp.Plan.Harness.Adapter)
			}
			instr := tc.read(t, sp.Dir)
			for _, want := range loop {
				if !strings.Contains(instr, want) {
					t.Errorf("the instruction does not carry %q", want)
				}
			}
			for _, s := range skills {
				if !strings.Contains(instr, "  - "+s+": \"") {
					t.Errorf("the skills line does not list %s", s)
				}
			}
		})
	}
}

// claudeSignIn is a policy that pays for Claude Code with the person's Claude
// subscription, the policy claude-image runs on.
func claudeSignIn() *policy.Policy {
	p := policy.Default()
	p.Model = policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-4-6", APIKeyKind: policy.KeyKindClaudeSignIn,
		ProxyAllow: policy.SignInProxyAllow(policy.KeyKindClaudeSignIn)}
	return p
}

// harnessInstructionFile is the instruction a host-style harness (Claude
// Code, Codex) reads from its plan file, in place of its own prompt.
func harnessInstructionFile(t *testing.T, planDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(planDir, "harness", "instruction.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

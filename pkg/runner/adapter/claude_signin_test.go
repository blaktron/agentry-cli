package adapter

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func claudeSignInInput(mode manifest.Mode) Input {
	in := conformanceInput(mode, false)
	in.Model = policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-4-6", APIKeyKind: policy.KeyKindClaudeSignIn}
	return in
}

// argValue is the value after flag in argv, and whether flag is there.
func argValue(argv []string, flag string) (string, bool) {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return "", i >= 0
	}
	return argv[i+1], true
}

// A run on Claude Code's own sign-in, in the image or the installed Claude
// Code, in either mode (plan model-sign-ins.md §4.4; agentry-cli#740):
// without --bare, which reads no OAuth credential; with no key variable at
// all, so nothing outranks the sign-in, and none of the person's
// inherited; with no settings loaded, so no apiKeyHelper; the gateway the
// only MCP server and no built-in tool; nonessential traffic, the
// autoupdater and claude.ai's connectors off; the egress proxy its route;
// and Claude Code's config directory the one thing of the sign-in it is
// handed, never read by the runner.
func TestAClaudeSignInRunIsHeldToClaudeCodesOwnSignIn(t *testing.T) {
	for _, name := range []string{ClaudeImageAdapterName, ClaudeAdapterName} {
		ad, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []manifest.Mode{manifest.ModeAutonomous, manifest.ModeInteractive} {
			what := name + "/" + string(mode)
			r, err := ad.Compile(claudeSignInInput(mode))
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			argv, env, proxy := r.Agent.Argv, r.Agent.Env, spec.ProxyURL()
			mcpPath, configDir := spec.PlanMount+"/"+claudeMCPFile, ClaudeImageConfigDir
			if r.Host != nil {
				argv, env, proxy = r.Host.Argv, r.Host.Env, spec.PHHostProxyURL
				mcpPath, configDir = claudeMCPFile, spec.PHModelSignIn
				if !slices.Equal(r.Host.InheritEnv, []string{"HOME", "PATH"}) {
					t.Errorf("%s: inherits %v of the person's environment", what, r.Host.InheritEnv)
				}
				// DR-26's refusal still holds on the host: without --bare
				// Claude Code reads the ancestors' CLAUDE.md.
				if !slices.Contains(r.Host.RefuseAncestorFiles, "CLAUDE.md") {
					t.Errorf("%s: the ancestor CLAUDE.md refusal is gone", what)
				}
			} else if r.Agent.Entrypoint != claudeImageBinary || !r.Agent.ReadOnlyRoot {
				t.Errorf("%s: the container is %+v", what, r.Agent)
			}
			if slices.Contains(argv, "--bare") {
				t.Errorf("%s: --bare reads no OAuth credential", what)
			}
			for flag, want := range map[string]string{"--setting-sources": "", "--tools": "", "--mcp-config": mcpPath, "--allowedTools": "mcp__gateway"} {
				if got, ok := argValue(argv, flag); !ok || got != want {
					t.Errorf("%s: %s %q, want %q", what, flag, got, want)
				}
			}
			if !slices.Contains(argv, "--strict-mcp-config") {
				t.Errorf("%s: no --strict-mcp-config", what)
			}
			var mcp claudeMCPConfig
			if err := json.Unmarshal(r.Files[claudeMCPFile], &mcp); err != nil || len(mcp.MCPServers) != 1 || mcp.MCPServers[claudeGatewayServer].URL == "" {
				t.Errorf("%s: mcp.json %s (%v)", what, r.Files[claudeMCPFile], err)
			}
			vals := map[string]string{}
			for _, e := range env {
				if e.FromHostEnv != "" || e.FromKeyring != "" || e.FromModelSignIn {
					t.Errorf("%s: %s is resolved at run time (%+v): a sign-in run carries no key", what, e.Name, e)
				}
				vals[e.Name] = e.Value
			}
			for _, unset := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN"} {
				if _, ok := vals[unset]; ok {
					t.Errorf("%s: %s is set", what, unset)
				}
			}
			for k, want := range map[string]string{
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1", "ENABLE_CLAUDEAI_MCP_SERVERS": "false",
				"HTTPS_PROXY": proxy, "HTTP_PROXY": proxy, "CLAUDE_CONFIG_DIR": configDir,
			} {
				if vals[k] != want {
					t.Errorf("%s: %s=%q, want %q", what, k, vals[k], want)
				}
			}
			if m := r.ModelSignIn; m == nil || *m != (spec.ModelSignInSpec{Kind: policy.KeyKindClaudeSignIn, Provider: policy.ProviderAnthropic, Delivery: spec.ModelSignInConfigDir, Path: configDir}) {
				t.Errorf("%s: modelSignIn %+v", what, r.ModelSignIn)
			}
			if (r.Session != nil) != (mode == manifest.ModeInteractive) {
				t.Errorf("%s: session %+v", what, r.Session)
			}
		}
	}
}

// The image runs on Claude Code's sign-in alone: an Anthropic key stays
// docker-agent's or the installed claude's. Neither adapter on a sign-in
// is pointed at another endpoint than Anthropic's, which would hand that
// endpoint the person's login.
func TestClaudeCodesSignInRunsOnNothingElse(t *testing.T) {
	img, _ := Get(ClaudeImageAdapterName)
	key := claudeSignInInput(manifest.ModeAutonomous)
	key.Model.APIKeyKind, key.Model.APIKeyEnv = policy.KeyKindAPI, "ANTHROPIC_API_KEY"
	var ref *Refusal
	if _, err := img.Compile(key); !errors.As(err, &ref) || !strings.Contains(err.Error(), "runs on your Claude subscription") {
		t.Fatalf("an API key in the image: %v", err)
	}
	for _, name := range []string{ClaudeImageAdapterName, ClaudeAdapterName} {
		ad, _ := Get(name)
		in := claudeSignInInput(manifest.ModeAutonomous)
		in.Harness.BaseURL = "https://gateway.example/anthropic"
		if _, err := ad.Compile(in); !errors.As(err, &ref) || !strings.Contains(err.Error(), "reaches Anthropic itself") {
			t.Errorf("%s with a base URL: %v", name, err)
		}
	}
	if !SpendsModelSignIn(img, policy.KeyKindClaudeSignIn) || SpendsModelSignIn(img, policy.KeyKindChatGPTPlan) {
		t.Fatal("the image's kinds")
	}
	if ClaudeImageConfigDir == "/" || strings.HasPrefix(ClaudeImageConfigDir, spec.PlanMount) {
		t.Fatal("the config directory overlaps the plan")
	}
}

// A rate_limit_event Claude Code reports as rejected, or one whose plan's
// included usage is spent, is the plan's limit with Claude Code's reset
// time (I9); allowed and allowed_warning are kept and nothing more.
func TestARejectedRateLimitEventIsThePlansLimit(t *testing.T) {
	ad := &claudeAdapter{}
	resets := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)
	for line, want := range map[string]Event{
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":` + strconv.FormatInt(resets.Unix(), 10) + `,"rateLimitType":"five_hour"},"session_id":"s"}`: {Kind: EventPlanLimit, ResetsAt: resets},
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","errorCode":"credits_required"}}`:                                                                       {Kind: EventPlanLimit},
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1760108400,"utilization":0.9}}`:                                                       {Kind: EventOther},
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`:                                                                                                       {Kind: EventOther},
	} {
		evs := ad.Events([]byte(line))
		if len(evs) != 1 || evs[0].Kind != want.Kind || !evs[0].ResetsAt.Equal(want.ResetsAt) || len(evs[0].Raw) == 0 {
			t.Errorf("%s: %+v", line, evs)
			continue
		}
		if want.Kind == EventPlanLimit && !strings.Contains(evs[0].Text, "Claude Code reported the plan's usage limit") {
			t.Errorf("%s: text %q", line, evs[0].Text)
		}
	}
	img := &claudeImageAdapter{}
	if evs := img.Events([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`)); len(evs) != 1 || evs[0].Kind != EventPlanLimit || !evs[0].ResetsAt.IsZero() {
		t.Fatalf("the image's reading: %+v", evs)
	}
}

// The image this adapter runs is the one harnessbuild builds, by one name.
func TestTheClaudeImageAdapterIsTheOneHarnessbuildBuilds(t *testing.T) {
	if ClaudeImageAdapterName != harnessbuild.ClaudeImageAdapter || !harnessbuild.Builds(ClaudeImageAdapterName) {
		t.Fatal("the adapter and the build name two images")
	}
	if _, ok := policy.Default().Images.HarnessImageFor(ClaudeImageAdapterName); !ok {
		t.Fatal("no default image for the adapter")
	}
}

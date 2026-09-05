// docker-agent adapter tests: the compiled agent.yaml must carry the
// gateway as its ONLY toolset (RQ1/RQ2 — no built-ins, ever), the token
// must ride as ${env.…} (never a value), and the event mapper must be
// tolerant of shapes it does not know.
package adapter

import (
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

func testInput() Input {
	return Input{
		Instruction:  "Do the thing.\n",
		Summary:      "a test agent",
		Model:        policy.Default().Model,
		HarnessImage: "docker/docker-agent:1.128.0",
		GatewayURL:   spec.GatewayURL(),
	}
}

func TestDockerAgentCompile(t *testing.T) {
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ad.Compile(testInput())
	if err != nil {
		t.Fatal(err)
	}
	y := string(res.Files["agent.yaml"])

	for _, want := range []string{
		"type: mcp",
		"url: http://gateway:8811/mcp",
		"transport_type: streamable",
		"Bearer ${env.MCP_GATEWAY_TOKEN}",
		"allow_private_ips: true",
		"version: \"" + HarnessConfigVersion + "\"", // schema of the pinned harness (#43)
		"safety: autonomous",                        // serve acp has no flag; the config carries the mode (DR-15)
		"Do the thing.",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("agent.yaml missing %q:\n%s", want, y)
		}
	}
	// No built-in toolsets may sneak in: the gateway is the whole tool
	// surface (RUNNER-PLAN §R10).
	for _, banned := range []string{"type: shell", "type: filesystem", "type: fetch", "type: script"} {
		if strings.Contains(y, banned) {
			t.Errorf("agent.yaml enables a built-in toolset %q:\n%s", banned, y)
		}
	}

	agent := res.Agent
	if !agent.StdinTask {
		t.Error("task must arrive on stdin")
	}
	joined := strings.Join(agent.Argv, " ")
	if !strings.Contains(joined, "--exec") || !strings.Contains(joined, "--json") {
		t.Errorf("argv lacks --exec/--json: %v", agent.Argv)
	}
	var hasToken, hasKey, hasProxy bool
	for _, e := range agent.Env {
		switch {
		case e.Name == spec.GatewayTokenEnv && e.FromRunToken:
			hasToken = true
		case e.Name == "ANTHROPIC_API_KEY" && e.FromHostEnv == "ANTHROPIC_API_KEY":
			hasKey = true
		case e.Name == "HTTPS_PROXY" && e.Value == spec.ProxyURL():
			hasProxy = true
		}
	}
	if !hasToken || !hasKey || !hasProxy {
		t.Errorf("agent env incomplete (token=%v key=%v proxy=%v): %+v", hasToken, hasKey, hasProxy, agent.Env)
	}
	if len(agent.Mounts) != 1 || agent.Mounts[0].Target != spec.PlanMount || !agent.Mounts[0].ReadOnly {
		t.Errorf("agent must mount only /plan read-only, got %+v", agent.Mounts)
	}
	if res.ResultKind != "text" {
		t.Errorf("result kind = %q, want text", res.ResultKind)
	}
}

func TestDockerAgentCompileWithSchema(t *testing.T) {
	ad, _ := Get(DefaultName)
	in := testInput()
	in.SchemaJSON = []byte(`{"type":"object"}`)
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultKind != "json" {
		t.Errorf("result kind = %q, want json", res.ResultKind)
	}
	if !strings.Contains(string(res.Files["agent.yaml"]), `{"type":"object"}`) {
		t.Error("schema not embedded in instruction")
	}
}

func TestDockerAgentEvents(t *testing.T) {
	ad, _ := Get(DefaultName)
	tests := []struct {
		name string
		line string
		want []EventKind
	}{
		{"tool call", `{"type":"tool_call","tool":"fs_read"}`, []EventKind{EventToolCall}},
		{"tool use variant", `{"type":"tool_use_started"}`, []EventKind{EventToolCall}},
		{"choice text", `{"type":"agent_choice","message":{"content":"hello"}}`, []EventKind{EventAssistantText}},
		{"content parts", `{"type":"agent_choice","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`, []EventKind{EventAssistantText}},
		{"top level text", `{"type":"delta","content":"chunk"}`, []EventKind{EventAssistantText}},
		{"usage nested", `{"type":"stream_stopped","usage":{"input_tokens":10,"output_tokens":5,"cost":0.01}}`, []EventKind{EventUsage}},
		{"error", `{"type":"error","message":{"content":"boom"}}`, []EventKind{EventError}},
		{"unknown", `{"type":"heartbeat"}`, []EventKind{EventOther}},
		{"not json", `plain text line`, []EventKind{EventAssistantText}},
		{"empty", ``, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evs := ad.Events([]byte(tc.line))
			if len(evs) != len(tc.want) {
				t.Fatalf("got %d events, want %d: %+v", len(evs), len(tc.want), evs)
			}
			for i, want := range tc.want {
				if evs[i].Kind != want {
					t.Errorf("event %d kind = %s, want %s", i, evs[i].Kind, want)
				}
			}
		})
	}

	// Text extraction from parts joins in order.
	evs := ad.Events([]byte(`{"type":"agent_choice","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`))
	if evs[0].Text != "ab" {
		t.Errorf("joined text = %q, want ab", evs[0].Text)
	}
	// Usage numbers survive.
	evs = ad.Events([]byte(`{"type":"stream_stopped","usage":{"input_tokens":10,"output_tokens":5,"cost":0.01}}`))
	if evs[0].InputTokens != 10 || evs[0].OutputTokens != 5 || evs[0].CostUSD != 0.01 {
		t.Errorf("usage = %+v", evs[0])
	}
}

func TestRegistry(t *testing.T) {
	if _, err := Get("docker-agent"); err != nil {
		t.Fatalf("default adapter missing: %v", err)
	}
	if _, err := Get("goose"); err == nil || !strings.Contains(err.Error(), "M-R4") {
		t.Errorf("planned adapter must point at M-R4, got %v", err)
	}
	if _, err := Get("nope"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown adapter must say unknown, got %v", err)
	}
}

// TestDockerAgentCompileInteractive: the interactive plan differs from the
// autonomous one only in argv, stdin and the Session block (plan §6.10).
func TestDockerAgentCompileInteractive(t *testing.T) {
	ad, _ := Get(DefaultName)
	auto, err := ad.Compile(testInput())
	if err != nil {
		t.Fatal(err)
	}
	in := testInput()
	in.Mode = manifest.ModeInteractive
	inter, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(inter.Files["agent.yaml"]) != string(auto.Files["agent.yaml"]) {
		t.Error("agent.yaml differs between modes")
	}
	if inter.Session == nil || inter.Session.Transport != "acp-stdio" || inter.Session.Safety != "autonomous" || inter.Session.SessionDB != spec.SessionMount+"/session.db" {
		t.Errorf("session = %+v", inter.Session)
	}
	if len(inter.Session.Mounts) != 1 || inter.Session.Mounts[0].Source != spec.PHSession || inter.Session.Mounts[0].Target != spec.SessionMount || inter.Session.Mounts[0].ReadOnly {
		t.Errorf("session mounts = %+v", inter.Session.Mounts)
	}
	if inter.Agent.StdinTask {
		t.Error("interactive plan must not read a task from stdin")
	}
	if got := strings.Join(inter.Agent.Argv, " "); got != "serve acp /plan/agent.yaml --session-db /session/session.db" {
		t.Errorf("argv = %q", got)
	}
	// Everything else identical.
	a, b := auto.Agent, inter.Agent
	a.Argv, b.Argv, a.StdinTask, b.StdinTask = nil, nil, false, false
	if !reflect.DeepEqual(a, b) {
		t.Errorf("container specs differ beyond argv/stdin:\n%+v\n%+v", a, b)
	}
	if auto.Session != nil {
		t.Error("autonomous plan carries a Session block")
	}
	in.Mode = "daemon"
	if _, err := ad.Compile(in); err == nil {
		t.Error("unknown mode accepted")
	}
}

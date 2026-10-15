package adapter

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

var update = flag.Bool("update", false, "rewrite the conformance goldens")

// Conformance: every registered adapter, given the same Input, compiles
// deterministically into a spec that keeps the adapter invariants, matches
// its golden, and turns its recorded harness output into the normalized
// events the engine needs. A new adapter is a member the moment it
// registers.

func conformanceInput(mode manifest.Mode, schema bool) Input {
	in := Input{
		Mode:         mode,
		Instruction:  "You are the conformance fixture. Read the inputs, write one report.",
		Summary:      "conformance fixture",
		Model:        policy.Model{Provider: "openai", Name: "fixture-model", APIKeyEnv: "FIXTURE_API_KEY", BaseURL: "https://model.example/v1"},
		HarnessImage: "harness:fixture",
		GatewayURL:   spec.GatewayURL(),
	}
	if schema {
		in.SchemaJSON = []byte(`{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`)
	}
	return in
}

type conformanceCase struct {
	name   string
	mode   manifest.Mode
	schema bool
	// signIn is a model sign-in kind the policy names instead of a key;
	// such a case runs for the adapters that can spend it.
	signIn string
}

var conformanceCases = []conformanceCase{
	{"autonomous", manifest.ModeAutonomous, false, ""},
	{"schema", manifest.ModeAutonomous, true, ""},
	{"interactive", manifest.ModeInteractive, false, ""},
	{"signin-autonomous", manifest.ModeAutonomous, false, policy.KeyKindChatGPTPlan},
	{"signin-interactive", manifest.ModeInteractive, false, policy.KeyKindChatGPTPlan},
	{"claude-signin-autonomous", manifest.ModeAutonomous, false, policy.KeyKindClaudeSignIn},
	{"claude-signin-interactive", manifest.ModeInteractive, false, policy.KeyKindClaudeSignIn},
	{"copilot-signin-autonomous", manifest.ModeAutonomous, false, policy.KeyKindCopilotSignIn},
	{"copilot-signin-interactive", manifest.ModeInteractive, false, policy.KeyKindCopilotSignIn},
}

// signInOnly are the adapters that run on a model sign-in and on no key
// (Claude Code's image: an Anthropic key stays docker-agent's or the
// installed claude's; Copilot CLI's), so the key cases are not theirs, by
// the kind they run on.
var signInOnly = map[string]string{ClaudeImageAdapterName: policy.KeyKindClaudeSignIn, CopilotImageAdapterName: policy.KeyKindCopilotSignIn}

// gatewayBySession are the adapters whose driver hands the harness the
// gateway's bearer and the plan over the session itself (Copilot CLI's
// session.create), so the agent container is given neither: no variable
// carries the bearer and nothing is mounted.
var gatewayBySession = map[string]bool{CopilotImageAdapterName: true}

// recordingOf names the adapter whose recorded harness output an adapter is
// held to, when it reads the same harness's stream with the same Events:
// the Claude Code image is Claude Code, read as the host claude adapter
// reads it.
var recordingOf = map[string]string{ClaudeImageAdapterName: ClaudeAdapterName}

// goldenView is what the golden records: plan content, not Go pointers.
type goldenView struct {
	Files       map[string]string     `json:"files"`
	Agent       spec.ContainerSpec    `json:"agent"`
	Host        *spec.HostProcessSpec `json:"host,omitempty"`
	Session     *spec.SessionSpec     `json:"session,omitempty"`
	OneTurn     *spec.SessionSpec     `json:"oneTurn,omitempty"`
	ModelSignIn *spec.ModelSignInSpec `json:"modelSignIn,omitempty"`
	Model       string                `json:"model"`
	ResultKind  string                `json:"resultKind"`
}

func view(r Result) goldenView {
	v := goldenView{Files: map[string]string{}, Agent: r.Agent, Host: r.Host, Session: r.Session, OneTurn: r.OneTurn,
		ModelSignIn: r.ModelSignIn, Model: r.Model, ResultKind: r.ResultKind}
	for name, b := range r.Files {
		v.Files[name] = string(b)
	}
	return v
}

func TestConformance(t *testing.T) {
	for _, name := range Names() {
		ad, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range conformanceCases {
				if tc.signIn != "" && !SpendsModelSignIn(ad, tc.signIn) || tc.signIn == "" && signInOnly[name] != "" {
					continue
				}
				t.Run(tc.name, func(t *testing.T) { conformCompile(t, ad, tc) })
			}
			t.Run("events", func(t *testing.T) { conformEvents(t, ad) })
		})
	}
}

func conformCompile(t *testing.T, ad Adapter, tc conformanceCase) {
	t.Helper()
	in := conformanceInput(tc.mode, tc.schema)
	if tc.signIn != "" {
		in.Model = policy.Model{Provider: policy.SignInProvider(tc.signIn), Name: "fixture-model", APIKeyKind: tc.signIn}
	}
	r1, err := ad.Compile(in)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	r2, err := ad.Compile(in)
	if err != nil {
		t.Fatalf("compile again: %v", err)
	}
	if !reflect.DeepEqual(view(r1), view(r2)) {
		t.Fatal("Compile is not deterministic for the same Input")
	}
	r := r1
	host := r.Host != nil
	if host == (r.Agent.Image != "") {
		t.Fatalf("exactly one of Agent.Image and Host must be set: image=%q host=%v", r.Agent.Image, host)
	}
	env := r.Agent.Env
	stdinTask := r.Agent.StdinTask
	if host {
		env = r.Host.Env
		stdinTask = r.Host.StdinTask
		if r.Host.Command == "" || filepath.IsAbs(r.Host.Command) {
			t.Errorf("host command must be a bare executable name, got %q", r.Host.Command)
		}
	}
	find := func(name string) (spec.EnvVar, bool) {
		for _, v := range env {
			if v.Name == name {
				return v, true
			}
		}
		return spec.EnvVar{}, false
	}
	// The proxy is the route; NO_PROXY keeps the gateway direct.
	wantProxy := spec.ProxyURL()
	wantNoProxy := spec.GatewayAlias
	if host {
		wantProxy, wantNoProxy = spec.PHHostProxyURL, "127.0.0.1"
	}
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY"} {
		v, ok := find(name)
		if !ok || v.Value != wantProxy {
			t.Errorf("%s = %+v, want value %q", name, v, wantProxy)
		}
	}
	if v, ok := find("NO_PROXY"); !ok || !strings.Contains(v.Value, wantNoProxy) {
		t.Errorf("NO_PROXY = %+v, want it to name %q", v, wantNoProxy)
	}
	// The token and the model key travel by name, or over the session
	// alone.
	if v, ok := find(spec.GatewayTokenEnv); gatewayBySession[ad.Name()] {
		for _, e := range env {
			if e.FromRunToken {
				t.Errorf("the gateway's bearer reaches %s over the session, and env %s carries it too", ad.Name(), e.Name)
			}
		}
	} else if !ok || !v.FromRunToken || v.Value != "" {
		t.Errorf("gateway token env = %+v, want FromRunToken", v)
	}
	// The VARIABLE the harness reads the key under is the harness's own
	// (docker-agent takes the policy's name, claude takes
	// ANTHROPIC_AUTH_TOKEN), so what the invariant pins is that the key is
	// resolved from the operator's environment at run time and that no
	// plan carries its value.
	keyByName := false
	for _, v := range env {
		if v.FromHostEnv == in.Model.APIKeyEnv && v.Value == "" {
			keyByName = true
		}
	}
	switch {
	case tc.signIn != "":
		// A run paid by a sign-in carries no key at all (I3), and reads
		// one token file the compiler mounts or the runner writes, or, for
		// Claude Code, its own config directory (I4).
		for _, v := range env {
			if v.FromHostEnv != "" || v.FromKeyring != "" {
				t.Errorf("a sign-in run is given the key variable %s", v.Name)
			}
		}
		delivery := spec.ModelSignInToken
		switch tc.signIn {
		case policy.KeyKindClaudeSignIn:
			delivery = spec.ModelSignInConfigDir
		case policy.KeyKindCopilotSignIn:
			delivery = spec.ModelSignInCallback
		}
		if m := r.ModelSignIn; m == nil || m.Kind != tc.signIn || m.Delivery != delivery {
			t.Errorf("modelSignIn = %+v, want a %s %s", m, tc.signIn, delivery)
		}
	case !keyByName:
		t.Errorf("no env var resolves the model key %s from the host environment: %+v", in.Model.APIKeyEnv, env)
	}
	for _, v := range env {
		if v.Value != "" && (strings.Contains(v.Value, "sk-") || strings.Contains(strings.ToLower(v.Name), "secret")) {
			t.Errorf("env %s carries a literal that looks like a secret", v.Name)
		}
	}
	// Gateway-only tools (the part a compile can show): the gateway is
	// named somewhere the harness reads, and no plan file carries a host
	// path.
	sawGateway := false
	for name, b := range r.Files {
		s := string(b)
		if strings.Contains(s, in.GatewayURL) || strings.Contains(s, spec.PHHostGatewayURL) {
			sawGateway = true
		}
		if strings.Contains(s, "/home/") || strings.Contains(s, "C:\\") {
			t.Errorf("plan file %s carries a host path", name)
		}
	}
	if !sawGateway {
		t.Error("no plan file names the gateway URL")
	}
	if gatewayBySession[ad.Name()] {
		if len(r.Agent.Mounts) != 0 {
			t.Errorf("%s's harness is handed its plan over the session, and its container mounts %+v", ad.Name(), r.Agent.Mounts)
		}
	} else if !host {
		ro := false
		for _, m := range r.Agent.Mounts {
			if m.Target == spec.PlanMount && m.ReadOnly {
				ro = true
			}
		}
		if !ro {
			t.Errorf("agent container must mount the plan read-only at %s: %+v", spec.PlanMount, r.Agent.Mounts)
		}
	}
	switch tc.mode {
	case manifest.ModeAutonomous:
		// The task goes in on stdin, or as the one turn of a session.
		if stdinTask == (r.OneTurn != nil) {
			t.Errorf("an autonomous run takes its task on stdin or as one session turn, exactly one: stdin=%v oneTurn=%+v", stdinTask, r.OneTurn)
		}
		if r.OneTurn != nil && r.OneTurn.Transport == "" {
			t.Error("a one-turn run names no transport")
		}
		if r.Session != nil {
			t.Error("autonomous plan carries a session block")
		}
	case manifest.ModeInteractive:
		if r.Session == nil || r.Session.Transport == "" {
			t.Errorf("interactive plan needs a session block with a transport: %+v", r.Session)
		}
	}
	wantKind := "text"
	if tc.schema {
		wantKind = "json"
	}
	if r.ResultKind != wantKind {
		t.Errorf("resultKind = %q, want %q", r.ResultKind, wantKind)
	}
	if r.Model == "" {
		t.Error("result names no model")
	}
	// Golden: the plan content is the contract with the harness.
	got, err := json.MarshalIndent(view(r), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "conformance", ad.Name(), tc.name+".json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // testdata
	if err != nil {
		t.Fatalf("golden %s: %v (run with -update to create it)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden %s differs from Compile output (run with -update after checking the diff):\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// conformEvents runs the adapter's recorded harness output through Events
// and checks the shape the engine relies on.
func conformEvents(t *testing.T, ad Adapter) {
	t.Helper()
	in := conformanceInput(manifest.ModeAutonomous, false)
	if kind := signInOnly[ad.Name()]; kind != "" {
		in.Model = policy.Model{Provider: policy.SignInProvider(kind), Name: "fixture-model", APIKeyKind: kind}
	}
	if r, err := ad.Compile(in); err == nil && r.OneTurn != nil {
		t.Skip("every run of this adapter is driven by its session driver, which normalizes the stream itself (TestCodexSession, TestACopilotSessionIsHandedItsTokenThroughTheCallback; the engine's TestACodexImageRunOnAChatGPTPlanIsOneAppServerTurn)")
	}
	recording := ad.Name()
	if of, ok := recordingOf[recording]; ok {
		recording = of
	}
	dir := filepath.Join("testdata", "conformance", recording)
	raw, err := os.ReadFile(filepath.Join(dir, "events.ndjson")) //nolint:gosec // testdata
	if err != nil {
		t.Fatalf("recorded harness output: %v (capture one with scripts/acceptance/runner-r4.sh)", err)
	}
	var expect struct {
		ToolCalls     int  `json:"toolCalls"`
		AssistantText bool `json:"assistantText"`
		Usage         bool `json:"usage"`
	}
	eb, err := os.ReadFile(filepath.Join(dir, "expect.json")) //nolint:gosec // testdata
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(eb, &expect); err != nil {
		t.Fatal(err)
	}
	var toolCalls int
	var text, usage bool
	for _, line := range bytes.Split(raw, []byte("\n")) {
		for _, ev := range ad.Events(line) {
			switch ev.Kind {
			case EventToolCall:
				toolCalls++
			case EventAssistantText:
				text = true
			case EventUsage:
				usage = true
			}
			if len(ev.Raw) == 0 && len(bytes.TrimSpace(line)) > 0 {
				t.Errorf("event %s dropped its raw line", ev.Kind)
			}
		}
	}
	if toolCalls != expect.ToolCalls || text != expect.AssistantText || usage != expect.Usage {
		t.Errorf("events: toolCalls=%d text=%v usage=%v, want %+v", toolCalls, text, usage, expect)
	}
}

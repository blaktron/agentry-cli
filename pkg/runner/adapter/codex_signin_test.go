package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// refusedTurn plays an app-server whose one turn the model's endpoint
// refuses with status, in codex 0.162.0's shapes (measured against a fake
// Responses endpoint), and returns the turn's events and the initialize
// request's clientInfo.
func refusedTurn(t *testing.T, signIn bool, status int) ([]Event, map[string]any, error) {
	t.Helper()
	ad, _ := Get(CodexImageAdapterName)
	stdio := newFakeStdio()
	drv, err := ad.Session(context.Background(), stdio, SessionOptions{ModelSignIn: signIn})
	if err != nil {
		t.Fatal(err)
	}
	variant, msg := "responseTooManyFailedAttempts", "exceeded retry limit, last status: 429 Too Many Requests"
	if status == 403 {
		variant, msg = "httpConnectionFailed", "unexpected status 403 Forbidden: This account is not eligible for ChatGPT plan usage in this app."
	}
	errObj, _ := json.Marshal(map[string]any{"message": msg, "codexErrorInfo": map[string]any{variant: map[string]any{"httpStatusCode": status}}})
	clientInfo := make(chan map[string]any, 1)
	go func() {
		for line := range stdio.lines {
			var m struct {
				ID     *int           `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			switch m.Method {
			case "initialize":
				ci, _ := m.Params["clientInfo"].(map[string]any)
				clientInfo <- ci
				stdio.say(`{"id":` + itoa(*m.ID) + `,"result":{"userAgent":"codex"}}`)
			case "thread/start":
				stdio.say(`{"id":` + itoa(*m.ID) + `,"result":{"thread":{"id":"t1"}}}`)
			case "turn/start":
				stdio.say(`{"id":` + itoa(*m.ID) + `,"result":{"turn":{"id":"u1","status":"inProgress"}}}`)
				stdio.say(`{"method":"error","params":{"error":` + string(errObj) + `,"willRetry":false,"threadId":"t1","turnId":"u1"}}`)
				stdio.say(`{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"u1","status":"failed","error":` + string(errObj) + `}}}`)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, perr := drv.Prompt(ctx, "go")
	done := make(chan []Event, 1)
	go func() {
		var evs []Event
		for ev := range drv.Events() {
			evs = append(evs, ev)
		}
		done <- evs
	}()
	_ = drv.Close()
	return <-done, <-clientInfo, perr
}

func eventsOf(evs []Event, kind EventKind) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// On a run paid by a sign-in, a 429 is the plan's limit, said once with
// codex's words (I9); on a key it is a rate limit, an error like any other.
func TestA429IsThePlansLimitOnASignInAlone(t *testing.T) {
	evs, ci, err := refusedTurn(t, true, 429)
	if err == nil {
		t.Fatal("a refused turn returned no error")
	}
	limits := eventsOf(evs, EventPlanLimit)
	if len(limits) != 1 || !strings.Contains(limits[0].Text, "last status: 429") || !limits[0].ResetsAt.IsZero() {
		t.Fatalf("plan-limit events %+v", limits)
	}
	if ci["name"] != "Agentry" {
		t.Errorf("clientInfo %v: OpenAI's plan usage wants the registered name, Agentry", ci)
	}
	evs, _, _ = refusedTurn(t, false, 429)
	if len(eventsOf(evs, EventPlanLimit)) != 0 || len(eventsOf(evs, EventError)) != 1 {
		t.Fatalf("a key run's 429: %+v", evs)
	}
}

// A 403 on a sign-in carries the advice to use an API key; on a key it does not.
func TestA403OnASignInSaysToUseAKey(t *testing.T) {
	evs, _, _ := refusedTurn(t, true, 403)
	errs := eventsOf(evs, EventError)
	if len(errs) != 1 || !strings.Contains(errs[0].Text, "not eligible") || !strings.Contains(errs[0].Text, "use an OpenAI API key") {
		t.Fatalf("errors %+v", errs)
	}
	evs, _, _ = refusedTurn(t, false, 403)
	if errs := eventsOf(evs, EventError); len(errs) != 1 || strings.Contains(errs[0].Text, "API key") {
		t.Fatalf("a key run's 403: %+v", errs)
	}
}

func signInInput(mode manifest.Mode) Input {
	in := conformanceInput(mode, false)
	in.Model = policy.Model{Provider: policy.ProviderOpenAI, Name: "fixture-model", APIKeyKind: policy.KeyKindChatGPTPlan}
	return in
}

// The installed Codex reads the token file with cat, which a Windows host
// lacks: there a ChatGPT plan is the image's.
func TestTheInstalledCodexRefusesAPlanOnWindows(t *testing.T) {
	prev := hostOS
	hostOS = "windows"
	t.Cleanup(func() { hostOS = prev })
	ad, _ := Get(CodexAdapterName)
	_, err := ad.Compile(signInInput(manifest.ModeAutonomous))
	var r *Refusal
	if !errors.As(err, &r) || !strings.Contains(err.Error(), "--adapter codex-image") {
		t.Fatalf("err %v", err)
	}
	// A key run on Windows compiles.
	if _, err := ad.Compile(conformanceInput(manifest.ModeAutonomous, false)); err != nil {
		t.Fatal(err)
	}
}

// A ChatGPT plan's endpoint is OpenAI's own: a policy pointing the codex
// adapters elsewhere is refused, as is a sign-in they cannot spend.
func TestTheCodexAdaptersRefuseWhatAPlanCannotBe(t *testing.T) {
	for _, name := range []string{CodexAdapterName, CodexImageAdapterName} {
		ad, _ := Get(name)
		in := signInInput(manifest.ModeAutonomous)
		in.Model.BaseURL = "https://proxy.example/v1"
		var r *Refusal
		if _, err := ad.Compile(in); !errors.As(err, &r) || !strings.Contains(err.Error(), "https://api.openai.com/v1 alone") {
			t.Errorf("%s with another endpoint: %v", name, err)
		}
		in = signInInput(manifest.ModeAutonomous)
		in.Model.Provider, in.Model.APIKeyKind = policy.ProviderAnthropic, policy.KeyKindClaudeSignIn
		if _, err := ad.Compile(in); !errors.As(err, &r) {
			t.Errorf("%s with a Claude sign-in: %v", name, err)
		}
	}
}

// The restart route starts codex on the token of the moment and resumes
// after a renewal: the container reads the file once in its shell, the
// host process is started with it, and the session says it restarts.
func TestTheRestartRouteReadsTheTokenOnceAtStart(t *testing.T) {
	restart := func(in Input) Input {
		in.Harness.SignInTokenRoute = policy.TokenRouteRestart
		return in
	}
	img, _ := Get(CodexImageAdapterName)
	r, err := img.Compile(restart(signInInput(manifest.ModeInteractive)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(r.Files[codexConfigFile])
	if !strings.Contains(r.Agent.Argv[1], `CODEX_ACCESS_TOKEN="$(cat /run/agentry/model-signin/token)"`) ||
		!strings.Contains(cfg, `env_key = "CODEX_ACCESS_TOKEN"`) || strings.Contains(cfg, ".auth]") ||
		r.Session == nil || !r.Session.RestartOnRenewal {
		t.Fatalf("container on the restart route: argv %q, session %+v\n%s", r.Agent.Argv, r.Session, cfg)
	}
	host, _ := Get(CodexAdapterName)
	r, err = host.Compile(restart(signInInput(manifest.ModeInteractive)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range r.Host.Env {
		if e.Name == "CODEX_ACCESS_TOKEN" && e.FromModelSignIn && e.Value == "" {
			found = true
		}
	}
	if !found || !r.Session.RestartOnRenewal {
		t.Fatalf("host on the restart route: env %+v, session %+v", r.Host.Env, r.Session)
	}
	// The default route reads the file whenever codex needs a token.
	r, _ = img.Compile(signInInput(manifest.ModeInteractive))
	if r.Session.RestartOnRenewal || !strings.Contains(string(r.Files[codexConfigFile]), `command = "/bin/cat"`) {
		t.Fatalf("the default route: %+v", r.Session)
	}
}

// The Codex image may run on an OpenAI API key, as a choice: the key by
// name, no sign-in, both modes app-server.
func TestTheCodexImageRunsOnAnAPIKeyByChoice(t *testing.T) {
	img, _ := Get(CodexImageAdapterName)
	r, err := img.Compile(conformanceInput(manifest.ModeAutonomous, false))
	if err != nil {
		t.Fatal(err)
	}
	if r.ModelSignIn != nil || r.OneTurn == nil || !strings.Contains(string(r.Files[codexConfigFile]), `env_key = "FIXTURE_API_KEY"`) {
		t.Fatalf("a key run: %+v", r)
	}
}

// docker-agent stays the default for every key; a ChatGPT plan runs on the
// Codex image, and a Claude subscription on the Claude Code image, unless
// the policy or the flag picks otherwise.
func TestTheDefaultAdapterFollowsThePlan(t *testing.T) {
	pol := policy.Default()
	if DefaultFor(pol) != DefaultName || DefaultFor(nil) != DefaultName {
		t.Fatal("a key's default is not docker-agent")
	}
	pol.Model = policy.Model{Provider: policy.ProviderOpenAI, Name: "m", APIKeyKind: policy.KeyKindChatGPTPlan}
	if DefaultFor(pol) != CodexImageAdapterName {
		t.Fatalf("a ChatGPT plan's default is %q", DefaultFor(pol))
	}
	pol.Model.APIKeyKind, pol.Model.Provider = policy.KeyKindClaudeSignIn, policy.ProviderAnthropic
	if DefaultFor(pol) != ClaudeImageAdapterName {
		t.Fatalf("a Claude sign-in's default is %q", DefaultFor(pol))
	}
	if p, ok := policy.Default().Images.HarnessImageFor(CodexImageAdapterName); !ok || !policy.PinnedByDigest(p.Ref) || p.Artifact != "" {
		t.Fatalf("the Codex image default %+v is not pinned by digest", p)
	}
	_ = spec.ModelSignInToken
}

// codex's own usage-limit variant, as an object or a bare name, is the
// plan's limit as a 429 is; a failed turn that says nothing is an error.
func TestCodexsUsageLimitVariantIsThePlansLimit(t *testing.T) {
	s := &codexSession{signIn: true}
	for _, info := range []string{`{"usageLimitExceeded":{"limitId":"weekly"}}`, `"usageLimitExceeded"`, `{"other":{"httpStatusCode":429}}`} {
		s.planLimited = false
		ev, ok := s.signInError(&codexError{Message: "limit", Info: json.RawMessage(info)}, nil)
		if !ok || ev.Kind != EventPlanLimit {
			t.Errorf("%s: %+v", info, ev)
		}
	}
	s.planLimited = false
	ev, _ := s.signInError(&codexError{Message: "exceeded retry limit, last status: 429 Too Many Requests"}, nil)
	if ev.Kind != EventPlanLimit {
		t.Errorf("the message alone: %+v", ev)
	}
	s.planLimited = false
	ev, _ = s.signInError(&codexError{Message: "boom", Info: json.RawMessage(`"internalServerError"`)}, nil)
	if ev.Kind != EventError {
		t.Errorf("another variant: %+v", ev)
	}
}

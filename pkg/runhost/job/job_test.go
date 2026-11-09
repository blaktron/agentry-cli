package job

import (
	"bytes"
	"maps"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

func validJob() *Job {
	return &Job{V: Version, ID: "0123456789abcdef", Device: "aaaaaaaaaaaaaaaa",
		Release: Release{Namespace: "acme", Name: "demo", Version: "1.0.0", Digest: "sha256:" + strings.Repeat("a", 64), Size: 10,
			Registry: "https://registry.test", State: StateUnsigned},
		Run: Run{AllowUnsigned: true, ModelKey: "k", Model: Model{Provider: "anthropic", Name: "m", APIKeyEnv: "ANTHROPIC_API_KEY"}}}
}

// A device names the variable its model key travels in; it can never name
// one the run's environment reserves, or one that is not a key's.
func TestAModelKeyNeverLandsInAReservedVariable(t *testing.T) {
	for _, ok := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "HF_TOKEN", "VULTR_INFERENCE_API_KEY", "MY_KEY"} {
		if !ValidKeyEnv(ok) {
			t.Errorf("%s refused", ok)
		}
	}
	for _, bad := range []string{"AGENTRY_HOSTED", "AGENTRY_RUN_GRANT", "AGENTRY_JOB_TOKEN", "AGENTRY_RUNNER_HOME_TOKEN",
		"SECUREAGENTRY_TOKEN", "LD_PRELOAD", "LD_LIBRARY_KEY", "PATH", "HOME", "DOCKER_HOST", "DOCKER_AUTH_TOKEN",
		"SSH_AUTH_TOKEN", "GIT_ASKPASS_TOKEN", "XDG_RUNTIME_KEY", "", "anthropic_api_key", "A=B_KEY"} {
		if ValidKeyEnv(bad) {
			t.Errorf("%q taken", bad)
		}
		j := validJob()
		j.Run.Model.APIKeyEnv = bad
		if err := j.Validate(); err == nil {
			t.Errorf("a job with %q validated", bad)
		}
	}
}

func TestAJobRoundTripsAndRefusesWhatARunhostDoesNotTake(t *testing.T) {
	var b bytes.Buffer
	if err := Write(&b, validJob()); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(&b); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Job){
		"a host harness":        func(j *Job) { j.Run.Adapter = "claude" },
		"another host harness":  func(j *Job) { j.Run.Adapter = "codex" },
		"an unknown adapter":    func(j *Job) { j.Run.Adapter = "opencode" },
		"another version":       func(j *Job) { j.V = 2 },
		"unsigned, no consent":  func(j *Job) { j.Run.AllowUnsigned = false },
		"verified, no envelope": func(j *Job) { j.Release.State = StateVerified },
		"a path for a name":     func(j *Job) { j.Release.Name = "../x" },
		"no model key":          func(j *Job) { j.Run.ModelKey = "" },
		"an id not hex":         func(j *Job) { j.ID = "../../etc/passwd0" },
		"a window no model has": func(j *Job) { j.Run.Model.ContextWindow = policy.MaxContextWindow + 1 },
	} {
		j := validJob()
		change(j)
		if err := j.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
	var unknown bytes.Buffer
	doc := []byte(`{"v":1,"surprise":true}`)
	unknown.Write([]byte{0, 0, 0, byte(len(doc))})
	unknown.Write(doc)
	if _, err := Read(&unknown); err == nil {
		t.Error("an unknown field was read")
	}
}

// The device policy's context window reaches the runhost's policy, so a model
// of the operator's own endpoint is sized there as on the device.
func TestAJobCarriesTheModelsContextWindow(t *testing.T) {
	m := policy.Model{Provider: "custom", Name: "qwen3", APIKeyEnv: "CUSTOM_API_KEY", BaseURL: "https://llm.example.net/v1", ContextWindow: 262144, ProxyAllow: []string{"llm.example.net:443"}}
	j := validJob()
	j.Run.Model = ModelOf(m)
	var b bytes.Buffer
	if err := Write(&b, j); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&b)
	if err != nil {
		t.Fatal(err)
	}
	if w := got.Run.Model.Policy().ContextWindow; w != 262144 {
		t.Fatalf("the runhost's policy has window %d", w)
	}
}

// What the run carries beside its release is held to what a secrets file, an
// argument and the payload can take.
func TestAJobRefusesCarriedValuesARunCannotTake(t *testing.T) {
	payload := &Payload{Size: 10, Digest: "sha256:" + strings.Repeat("b", 64)}
	ok := validJob()
	ok.Run.Secrets = map[string]string{"API_TOKEN": "v"}
	ok.Run.Intake = map[string]string{"topic": "birds"}
	ok.Run.IntakeFiles = map[string]string{"doc": "intake/doc/brief.md"}
	ok.Run.Inputs = []string{"inputs/a.txt"}
	ok.Run.Knowledge = []string{"knowledge/pack"}
	ok.Run.Voice, ok.Run.ReportDesign = "voice/VOICE.md", "design/report-design.json"
	ok.Run.Payload = payload
	if err := ok.Validate(); err != nil {
		t.Fatalf("a whole job: %v", err)
	}
	for name, change := range map[string]func(*Job){
		"a secret over two lines":         func(j *Job) { j.Run.Secrets = map[string]string{"A": "x\nB=y"} },
		"a secret with a space at an end": func(j *Job) { j.Run.Secrets = map[string]string{"A": " x"} },
		"a secret's name with =":          func(j *Job) { j.Run.Secrets = map[string]string{"A=B": "x"} },
		"a secret named as the model key": func(j *Job) { j.Run.Secrets = map[string]string{"ANTHROPIC_API_KEY": "x"} },
		"an intake id with a path":        func(j *Job) { j.Run.Intake = map[string]string{"../x": "y"} },
		"an answer twice":                 func(j *Job) { j.Run.Intake = map[string]string{"doc": "y"} },
		"an intake file of another id":    func(j *Job) { j.Run.IntakeFiles = map[string]string{"doc": "intake/other/x"} },
		"an input that climbs":            func(j *Job) { j.Run.Inputs = []string{"inputs/../x"} },
		"an input in a folder":            func(j *Job) { j.Run.Inputs = []string{"inputs/a/b"} },
		"a pack outside knowledge":        func(j *Job) { j.Run.Knowledge = []string{"inputs/pack"} },
		"a voice elsewhere":               func(j *Job) { j.Run.Voice = "inputs/VOICE.md" },
		"a theme beside a design":         func(j *Job) { j.Run.ReportTheme = "plain" },
		"files and no payload":            func(j *Job) { j.Run.Payload = nil },
		"a payload with no digest":        func(j *Job) { j.Run.Payload = &Payload{Size: 1} },
		"a source token over two lines":   func(j *Job) { j.Run.SourceTokens = map[string]string{"repo": "a\nb"} },
		"a connection with no token":      func(j *Job) { j.Run.Connections = map[string]Connection{"srv": {}} },
		"a destination with no secret":    func(j *Job) { j.Run.Destinations = []Destination{{Name: "hook", Kind: "webhook"}} },
	} {
		j := validJob()
		*j = *ok
		j.Run.Secrets, j.Run.Intake, j.Run.IntakeFiles = maps.Clone(ok.Run.Secrets), maps.Clone(ok.Run.Intake), maps.Clone(ok.Run.IntakeFiles)
		change(j)
		if err := j.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

// A model paid with Agentry credits carries no key: its job names a
// registry's model endpoint and nothing a key travels in, and its policy
// reads no key.
func TestACreditsModelCarriesNoKey(t *testing.T) {
	credits := func(j *Job) {
		j.Run.ModelKey = ""
		j.Run.Model = Model{Provider: "agentry", Name: "claude-sonnet-5-5", BaseURL: "https://registry.test/api/v1/x/inference/v1"}
	}
	j := validJob()
	credits(j)
	if err := j.Validate(); err != nil {
		t.Fatalf("a credits job: %v", err)
	}
	if p := j.Run.Model.Policy(); p.APIKeySource != "" || p.APIKeyEnv != "" {
		t.Fatalf("a credits model's policy reads a key: %+v", p)
	}
	for name, change := range map[string]func(*Job){
		"a key":                func(j *Job) { j.Run.ModelKey = "k" },
		"a key variable":       func(j *Job) { j.Run.Model.APIKeyEnv = "ANTHROPIC_API_KEY" },
		"a sign-in kind":       func(j *Job) { j.Run.Model.APIKeyKind = "subscription" },
		"no model endpoint":    func(j *Job) { j.Run.Model.BaseURL = "" },
		"another endpoint":     func(j *Job) { j.Run.Model.BaseURL = "https://registry.test/v1" },
		"plain http elsewhere": func(j *Job) { j.Run.Model.BaseURL = "http://registry.test/api/v1/x/inference/v1" },
	} {
		j := validJob()
		credits(j)
		change(j)
		if err := j.Validate(); err == nil {
			t.Errorf("a credits job with %s: validated", name)
		}
	}
}

// A model paid by a plan's sign-in carries no key: the runhost keeps the
// sign-in, and its policy reads no key.
func TestASignInModelCarriesNoKey(t *testing.T) {
	signIn := func(j *Job) {
		j.Run.ModelKey = ""
		j.Run.Model = Model{Provider: "anthropic", Name: "sonnet", APIKeyKind: "claude-signin",
			ProxyAllow: []string{"api.anthropic.com:443", "platform.claude.com:443"}}
	}
	for _, m := range []Model{
		{Provider: "anthropic", Name: "sonnet", APIKeyKind: "claude-signin"},
		{Provider: "openai", Name: "gpt-5.5", APIKeyKind: "chatgpt-plan"},
		{Provider: "copilot", Name: "auto", APIKeyKind: "copilot-signin"},
	} {
		j := validJob()
		j.Run.ModelKey, j.Run.Model = "", m
		if err := j.Validate(); err != nil {
			t.Errorf("a %s job: %v", m.APIKeyKind, err)
		}
		if p := j.Run.Model.Policy(); p.APIKeySource != "" || p.APIKeyEnv != "" {
			t.Errorf("a %s model's policy reads a key: %+v", m.APIKeyKind, p)
		}
	}
	for name, change := range map[string]func(*Job){
		"a key":              func(j *Job) { j.Run.ModelKey = "k" },
		"a key variable":     func(j *Job) { j.Run.Model.APIKeyEnv = "ANTHROPIC_API_KEY" },
		"another provider":   func(j *Job) { j.Run.Model.Provider = "openai" },
		"a host harness":     func(j *Job) { j.Run.Adapter = "claude" },
		"an unknown adapter": func(j *Job) { j.Run.Adapter = "aider" },
	} {
		j := validJob()
		signIn(j)
		change(j)
		if err := j.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
	for _, a := range ImageAdapters {
		j := validJob()
		signIn(j)
		j.Run.Adapter = a
		if err := j.Validate(); err != nil {
			t.Errorf("the image adapter %s: %v", a, err)
		}
	}
}

// No harness runs as a host process on a runhost: the installed claude and
// codex become their images, image adapters stay, and any other is refused.
func TestARunhostRunsTheImageOfAHostHarness(t *testing.T) {
	for asked, want := range map[string]struct {
		name   string
		mapped bool
	}{
		"":              {"", false},
		"claude":        {AdapterClaudeImage, true},
		"codex":         {AdapterCodexImage, true},
		"claude-image":  {AdapterClaudeImage, false},
		"codex-image":   {AdapterCodexImage, false},
		"copilot-image": {AdapterCopilot, false},
		"docker-agent":  {AdapterDockerAgent, false},
	} {
		got, mapped, err := RunhostAdapter(asked)
		if err != nil || got != want.name || mapped != want.mapped {
			t.Errorf("%q: %q %v %v", asked, got, mapped, err)
		}
	}
	for _, bad := range []string{"opencode", "copilot", "goose", "claude-image "} {
		if _, _, err := RunhostAdapter(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}

func TestADesktopBindingIsHeldToWhatAJobCanCarry(t *testing.T) {
	ok := &Desktop{Run: "0123abcd", Bound: desktop.Bound{Token: "tok-1", Servers: map[string][]desktoplink.Tool{"blender": nil}}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]*Desktop{
		"no token":     {Run: "r", Bound: desktop.Bound{Servers: map[string][]desktoplink.Tool{"blender": nil}}},
		"a token line": {Run: "r", Bound: desktop.Bound{Token: "a\nb", Servers: map[string][]desktoplink.Tool{"blender": nil}}},
		"no run":       {Bound: desktop.Bound{Token: "t", Servers: map[string][]desktoplink.Tool{"blender": nil}}},
		"a path run":   {Run: "../x", Bound: desktop.Bound{Token: "t", Servers: map[string][]desktoplink.Tool{"blender": nil}}},
		"nothing":      {Run: "r", Bound: desktop.Bound{Token: "t"}},
		"bad server":   {Run: "r", Bound: desktop.Bound{Token: "t", Servers: map[string][]desktoplink.Tool{"a b": nil}}},
	} {
		if err := d.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A job on one of the runhost's own saved models names it by id, or the
// runhost's default, and carries no model and no key: the runhost holds
// them. Anything carried beside it is refused.
func TestAJobOnARunhostsSavedModelCarriesNoModelAndNoKey(t *testing.T) {
	for _, ref := range []string{"0a0b0c0d", RunhostDefault} {
		j := validJob()
		j.Run.Model, j.Run.ModelKey, j.Run.RunhostModel = Model{}, "", ref
		if err := j.Validate(); err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
	for name, change := range map[string]func(*Job){
		"a key":             func(j *Job) { j.Run.ModelKey = "sk-x" },
		"a model":           func(j *Job) { j.Run.Model = Model{Provider: "anthropic", Name: "m"} },
		"a name, not an id": func(j *Job) { j.Run.RunhostModel = "My Model" },
	} {
		j := validJob()
		j.Run.Model, j.Run.ModelKey, j.Run.RunhostModel = Model{}, "", "0a0b0c0d"
		change(j)
		if err := j.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

// An endpoint of the person's own that takes no key travels with its
// address and no key; one given a key, or no address, is refused.
func TestAnEndpointThatTakesNoKeyTravelsWithNone(t *testing.T) {
	j := validJob()
	j.Run.ModelKey = ""
	j.Run.Model = Model{Provider: policy.ProviderCustom, ProviderID: "custom-lab", Name: "qwen3", BaseURL: "http://lab:8000/v1", ProxyAllow: []string{"lab:8000"},
		SavedModelID: "0a0b0c0d", SavedModelName: "Qwen"}
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	if pm := j.Run.Model.Policy(); pm.APIKeySource != "" || pm.APIKeyEnv != "" || !policy.TakesNoKey(policy.HarnessPolicy{}, pm) || pm.KeyProvider() != "custom-lab" {
		t.Fatalf("the runhost's model %+v", pm)
	}
	id, pr, sm, ok := j.Run.Model.Saved()
	if !ok || id != "custom-lab" || sm.ID != "0a0b0c0d" || sm.Name != "Qwen" || pr.BaseURL != "http://lab:8000/v1" {
		t.Fatalf("the saved model %s %+v %+v", id, pr, sm)
	}
	keyed := *j
	keyed.Run.ModelKey = "sk-x"
	if err := keyed.Validate(); err == nil {
		t.Error("an endpoint that takes no key was sent one")
	}
	noAddr := *j
	noAddr.Run.Model.BaseURL = ""
	if err := noAddr.Validate(); err == nil {
		t.Error("an endpoint with no address validated")
	}
	for _, bad := range []func(*Model){
		func(m *Model) { m.ProviderID = "openai" },
		func(m *Model) { m.SavedModelName = "" },
		func(m *Model) { m.SavedModelID = "Not An ID" },
	} {
		k := *j
		bad(&k.Run.Model)
		if err := k.Validate(); err == nil {
			t.Errorf("%+v validated", k.Run.Model)
		}
	}
}

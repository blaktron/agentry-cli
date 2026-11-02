package job

import (
	"bytes"
	"maps"
	"strings"
	"testing"
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

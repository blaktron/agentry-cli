package job

import (
	"bytes"
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
		"an adapter":            func(j *Job) { j.Run.Adapter = "claude" },
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

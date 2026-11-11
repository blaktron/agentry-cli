package adapter

import (
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"gopkg.in/yaml.v3"
)

func ownModel(name string, window int64) policy.Model {
	return policy.Model{Provider: "custom", Name: name, APIKeyEnv: "CUSTOM_API_KEY", BaseURL: "http://127.0.0.1:1234/v1", ContextWindow: window, ProxyAllow: []string{"127.0.0.1:1234"}}
}

// renderedModel is the model block of the compiled agent.yaml.
func renderedModel(t *testing.T, m policy.Model) map[string]any {
	t.Helper()
	ad, err := Get(DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	in := testInput()
	in.Model = m
	res, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Models map[string]map[string]any `yaml:"models"`
	}
	if err := yaml.Unmarshal(res.Files["agent.yaml"], &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Models["main"]
}

func opts(t *testing.T, got map[string]any) map[string]any {
	t.Helper()
	o, _ := got["provider_opts"].(map[string]any)
	return o
}

// The values are those read from each family's card on ProfilesCheckedOn.
func TestProfileTableValues(t *testing.T) {
	if ProfilesCheckedOn != "2026-11-10" {
		t.Fatalf("profiles checked on %s", ProfilesCheckedOn)
	}
	cases := []struct {
		name                     string
		temp, topP               float64
		topK                     int64
		presence                 float64
		cap                      int64
		enableThinking, preserve bool
		replay                   bool
	}{
		{"NVIDIA-Nemotron-3.5-Super-VL-120B-A12B-MLX-4bit", 1.0, 0.95, 0, 0, 16384, true, false, true},
		{"nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-BF16", 1.0, 0.95, 0, 0, 32000, true, false, true},
		{"NVIDIA-Nemotron-3-Ultra-550B-A55B", 1.0, 0.95, 0, 0, 32000, true, false, true},
		{"NVIDIA-Nemotron-3.5-Lightning-30B-A3B-NVFP4", 1.0, 0.95, 0, 0, 16000, true, false, true},
		{"NVIDIA-Nemotron-3.5-Bolt-74B-A9B", 1.0, 0.95, 0, 0, 16384, true, false, true},
		{"NVIDIA-Nemotron-3-Nano-30B-A3B-BF16", 0.6, 0.95, 0, 0, 10000, true, false, true},
		{"NVIDIA-Nemotron-3-Nano-4B-BF16", 0.6, 0.95, 0, 0, 16384, true, false, true},
		{"Qwen3.5-35B-A3B", 1.0, 0.95, 20, 1.5, 81920, true, false, false},
		{"qwen/Qwen3.6-35B-A3B", 1.0, 0.95, 20, 1.5, 81920, true, true, true},
	}
	for _, c := range cases {
		p := profileFor(c.name)
		if p == nil {
			t.Errorf("%s: no profile", c.name)
			continue
		}
		got := struct {
			temp, topP, presence float64
			topK, cap            int64
			et, pt, replay       bool
		}{*p.temperature, *p.topP, 0, 0, p.answerLimit(0), *p.enableThinking, p.preserveThinking != nil && *p.preserveThinking, p.replayReasoning != nil && *p.replayReasoning}
		if p.presencePenalty != nil {
			got.presence = *p.presencePenalty
		}
		if p.topK != nil {
			got.topK = *p.topK
		}
		want := got
		want.temp, want.topP, want.topK, want.presence, want.cap, want.et, want.pt, want.replay = c.temp, c.topP, c.topK, c.presence, c.cap, c.enableThinking, c.preserve, c.replay
		if got != want {
			t.Errorf("%s: %+v, want %+v", c.name, got, want)
		}
	}
}

// Families whose reasoning is switched by a line of the system prompt, and
// families without a profile, render nothing new.
func TestProfileFamiliesWithoutOne(t *testing.T) {
	for _, n := range []string{
		"NVIDIA-Nemotron-Nano-9B-v2", "nvidia/NVIDIA-Nemotron-Nano-12B-v2",
		"Llama-3.3-Nemotron-Super-49B-v1.5", "Llama-3.1-Nemotron-Ultra-253B-v1",
		"gpt-oss-120b", "some-unknown-model", "",
	} {
		if profileFor(n) != nil {
			t.Errorf("%q has a profile", n)
		}
	}
}

func TestProfileRendersOnOwnEndpointOnly(t *testing.T) {
	m := renderedModel(t, ownModel("NVIDIA-Nemotron-3.5-Super-VL-120B-A12B-MLX-4bit", 262144))
	want := map[string]any{"temperature": 1, "top_p": 0.95, "max_tokens": 16384}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	o := opts(t, m)
	if o["replay_reasoning"] != true {
		t.Errorf("replay_reasoning = %v", o["replay_reasoning"])
	}
	kw := o["extra_body"].(map[string]any)["chat_template_kwargs"]
	if !reflect.DeepEqual(kw, map[string]any{"enable_thinking": true}) {
		t.Errorf("chat_template_kwargs = %v", kw)
	}

	hosted := policy.Model{Provider: "openai", Name: "NVIDIA-Nemotron-3-Super", APIKeyEnv: "OPENAI_API_KEY", ProxyAllow: []string{"api.openai.com:443"}}
	h := renderedModel(t, hosted)
	for _, k := range []string{"temperature", "top_p", "max_tokens", "presence_penalty", "provider_opts"} {
		if _, ok := h[k]; ok {
			t.Errorf("hosted model renders %s", k)
		}
	}
}

func TestProfileAnswerCapIsAQuarterOfASmallWindow(t *testing.T) {
	m := renderedModel(t, ownModel("NVIDIA-Nemotron-3-Super-120B-A12B", 40960))
	if m["max_tokens"] != 10240 {
		t.Errorf("max_tokens = %v, want a quarter of the window", m["max_tokens"])
	}
}

func TestProfileUnknownFamilyRendersNothingNew(t *testing.T) {
	m := renderedModel(t, ownModel("some-unknown-model", 131072))
	for _, k := range []string{"temperature", "top_p", "max_tokens", "presence_penalty"} {
		if _, ok := m[k]; ok {
			t.Errorf("renders %s", k)
		}
	}
	if want := map[string]any{"context_size": 131072, "stream_idle_timeout": "30m"}; !reflect.DeepEqual(opts(t, m), want) {
		t.Errorf("provider_opts %v, want %v", opts(t, m), want)
	}
}

// The person's keys win key by key; the family's other keys stay. An unknown
// family renders what the person set, with their max_tokens alone.
func TestProfilePolicyKeysOverrideKeyByKey(t *testing.T) {
	temp, topK, minP, rep := 0.2, int64(40), 0.05, 1.1
	pm := ownModel("Qwen3.6-35B-A3B", 262144)
	pm.Sampling = &policy.Sampling{Temperature: &temp, TopK: &topK, MinP: &minP, RepetitionPenalty: &rep}
	pm.MaxOutputTokens = 4096
	m := renderedModel(t, pm)
	if m["temperature"] != 0.2 || m["top_p"] != 0.95 || m["presence_penalty"] != 1.5 || m["max_tokens"] != 4096 {
		t.Errorf("model keys %v", m)
	}
	o := opts(t, m)
	if o["top_k"] != 40 || o["min_p"] != 0.05 || o["repetition_penalty"] != 1.1 {
		t.Errorf("provider_opts %v", o)
	}

	um := ownModel("some-unknown-model", 0)
	um.Sampling = &policy.Sampling{Temperature: &temp}
	um.MaxOutputTokens = 2048
	got := renderedModel(t, um)
	if got["temperature"] != 0.2 || got["max_tokens"] != 2048 {
		t.Errorf("unknown family with keys set: %v", got)
	}
	if _, ok := got["top_p"]; ok {
		t.Errorf("unknown family renders top_p")
	}
}

func TestProfilePlanBytesAreDeterministic(t *testing.T) {
	ad, _ := Get(DefaultName)
	in := testInput()
	in.Model = ownModel("Qwen3.6-35B-A3B", 262144)
	first, err := ad.Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, _ := ad.Compile(in)
		if string(again.Files["agent.yaml"]) != string(first.Files["agent.yaml"]) {
			t.Fatal("agent.yaml differs between compiles")
		}
	}
	if !strings.Contains(string(first.Files["agent.yaml"]), "preserve_thinking: true") {
		t.Error("Qwen3.6 does not send preserve_thinking")
	}
}

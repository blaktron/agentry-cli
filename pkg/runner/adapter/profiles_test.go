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

// shape is a profile with its values spelled out, nil for a key not sent.
type shape struct {
	temp, topP, presence, rep *float64
	topK                      *int64
	cap                       int64
	enable, preserve, replay  *bool
}

func shapeOf(p *profile) shape {
	return shape{p.temperature, p.topP, p.presencePenalty, p.repetitionPenalty, p.topK, p.answerCap,
		p.enableThinking, p.preserveThinking, p.replayReasoning}
}

// Each family's values as its card gives them on ProfilesCheckedOn; a family
// whose card states no output length has no cap, and a family whose template
// default is right sends no kwarg.
func TestProfileFamilyValues(t *testing.T) {
	T := ptrB(true)
	cases := []struct {
		name string
		want shape
	}{
		{"Qwen3.8-27B", shape{temp: ptrF(1), topP: ptrF(0.95), topK: ptrI(20), presence: ptrF(0), cap: 393216, enable: T, replay: T}},
		{"Qwen3.8-Flash-Next", shape{temp: ptrF(1), topP: ptrF(0.95), topK: ptrI(20), presence: ptrF(0), cap: 393216, enable: T, replay: T}},
		{"Qwen/Qwen3-Coder-480B-A35B-Instruct", shape{temp: ptrF(0.7), topP: ptrF(0.8), topK: ptrI(20), rep: ptrF(1.05), cap: 65536}},
		{"Qwen3-Coder-Next", shape{temp: ptrF(1), topP: ptrF(0.95), topK: ptrI(40), cap: 65536}},
		{"DeepSeek-V4.1-Flash-1017", shape{temp: ptrF(1), topP: ptrF(1), cap: 262144, replay: T}},
		{"DeepSeek-R1-0528", shape{temp: ptrF(0.6), topP: ptrF(0.95)}},
		{"DeepSeek-V3.2", shape{temp: ptrF(0.6), topP: ptrF(0.95)}},
		{"GLM-5.3", shape{temp: ptrF(1), topP: ptrF(0.95)}},
		{"GLM-5.3-Flash", shape{temp: ptrF(1), topP: ptrF(0.95)}},
		{"GLM-4.7", shape{temp: ptrF(1), topP: ptrF(0.95)}},
		{"GLM-4.6", shape{temp: ptrF(1), topP: ptrF(0.95)}},
		{"Kimi-K3.1", shape{temp: ptrF(1), topP: ptrF(0.95), replay: T}},
		{"Kimi-K2.6", shape{temp: ptrF(1), topP: ptrF(0.95), replay: T}},
		{"MiniMax-M2.7", shape{temp: ptrF(1), topP: ptrF(0.95), topK: ptrI(40), replay: T}},
		{"MiniMax-M3.1-Flash", shape{temp: ptrF(1), topP: ptrF(0.95), replay: T}},
		{"Magistral-Small-2509", shape{temp: ptrF(0.7), topP: ptrF(0.95), cap: 131072}},
		{"Mistral-Small-4-119B-2603", shape{temp: ptrF(0.7)}},
		{"Mistral-Large-4", shape{temp: ptrF(1), topP: ptrF(0.95)}},
		{"Devstral-2-123B-Instruct-2512", shape{temp: ptrF(0.15)}},
		{"gemma-4-31B-it", shape{temp: ptrF(1), topP: ptrF(0.95), topK: ptrI(64)}},
		{"granite-4.2-30b", shape{temp: ptrF(1), topP: ptrF(0.95)}},
	}
	for _, c := range cases {
		p := profileFor(c.name)
		if p == nil {
			t.Errorf("%s: no profile", c.name)
			continue
		}
		if got := shapeOf(p); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Gemma 4 ships with thinking off and its profile leaves it off.
func TestProfileGemmaThinkingStaysAtItsDefault(t *testing.T) {
	if p := profileFor("gemma-4-31B-it"); p.enableThinking != nil || p.preserveThinking != nil {
		t.Error("gemma-4 sends a thinking kwarg")
	}
}

// Families whose card states no sampling render nothing.
func TestProfileCardsWithoutSamplingRenderNothing(t *testing.T) {
	for _, n := range []string{"gpt-oss-120b", "gpt-oss-20b", "Llama-4-Scout-17B-16E-Instruct", "Llama-4-Maverick", "Mistral-7B-Instruct", "DeepSeek-V4-Pro-0813"} {
		m := renderedModel(t, ownModel(n, 131072))
		for _, k := range []string{"temperature", "top_p", "max_tokens", "presence_penalty"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s renders %s", n, k)
			}
		}
	}
}

// A name that fits two families takes the more specific one.
func TestProfileMoreSpecificNameWins(t *testing.T) {
	cases := []struct {
		specific, general string
		topK              int64
	}{
		{"Qwen3-Coder-Next-80B-A3B", "Qwen3-Coder-30B-A3B-Instruct", 40},
		{"Qwen3-Coder-30B-A3B-Instruct", "Qwen3.6-35B-A3B", 20},
	}
	for _, c := range cases {
		s, g := profileFor(c.specific), profileFor(c.general)
		if s == nil || g == nil || reflect.DeepEqual(shapeOf(s), shapeOf(g)) {
			t.Errorf("%s and %s share a profile", c.specific, c.general)
		}
	}
	// Coder-Next has no thinking switch; the Qwen3.x models have one.
	if profileFor("Qwen3-Coder-Next").enableThinking != nil || profileFor("Qwen3.5-35B-A3B").enableThinking == nil {
		t.Error("Qwen3-Coder-Next took a Qwen3.x profile")
	}
	// Mistral Large, Small, Magistral and Devstral each have their own.
	seen := map[float64]string{}
	for _, n := range []string{"Mistral-Large-4", "Mistral-Small-4-119B", "Magistral-Small-2509", "Devstral-2-123B"} {
		p := profileFor(n)
		if p == nil {
			t.Fatalf("%s has no profile", n)
		}
		key := *p.temperature*1000 + float64(p.answerCap)
		if prev, dup := seen[key]; dup {
			t.Errorf("%s and %s share a profile", n, prev)
		}
		seen[key] = n
	}
	// Nemotron 3 and Llama-Nemotron: the Llama models have none.
	if profileFor("Llama-3.3-Nemotron-Super-49B-v1.5") != nil || profileFor("NVIDIA-Nemotron-3-Super-120B") == nil {
		t.Error("Nemotron match crosses into Llama-Nemotron")
	}
	// DeepSeek V4.1 against R1 and V3.
	if a, b := profileFor("DeepSeek-V4.1-Flash"), profileFor("DeepSeek-V3.1"); *a.topP != 1 || *b.topP != 0.95 {
		t.Error("DeepSeek V4.1 and V3 share a profile")
	}
	// MiniMax M2.7 against M3.
	if profileFor("MiniMax-M2.7").topK == nil || profileFor("MiniMax-M3.1").topK != nil {
		t.Error("MiniMax M2.7 and M3 share a profile")
	}
}

// A repetition penalty from a card reaches provider_opts.
func TestProfileRepetitionPenaltyRenders(t *testing.T) {
	m := renderedModel(t, ownModel("Qwen3-Coder-30B-A3B-Instruct", 262144))
	if o := opts(t, m); o["repetition_penalty"] != 1.05 || o["top_k"] != 20 {
		t.Errorf("provider_opts %v", o)
	}
	if _, ok := m["presence_penalty"]; ok {
		t.Error("Qwen3-Coder renders a presence penalty")
	}
	// A family without a card length has no cap until the person sets one.
	g := renderedModel(t, ownModel("GLM-5.3", 131072))
	if _, ok := g["max_tokens"]; ok {
		t.Error("GLM renders a cap")
	}
	pm := ownModel("GLM-5.3", 131072)
	pm.MaxOutputTokens = 4096
	if got := renderedModel(t, pm)["max_tokens"]; got != 4096 {
		t.Errorf("max_tokens = %v", got)
	}
}

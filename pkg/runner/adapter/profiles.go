package adapter

import (
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// ProfilesCheckedOn is the date the values in profileTable were read from
// each family's model card and chat template.
const ProfilesCheckedOn = "2026-11-10"

// defaultAnswerCap is the answer cap of a Nemotron model whose card states
// no output length.
const defaultAnswerCap = 16384

// profile is one open-weight family's reasoning profile: the sampling its
// maker recommends for a model that reasons, the longest answer its card
// shows, and how reasoning is switched on and carried between turns. A nil
// pointer is a value the card does not state, which the server's own default
// then supplies. A family whose card states no output length has no answer
// cap (answerCap 0), so a run on it is capped only when the person asks.
type profile struct {
	temperature       *float64
	topP              *float64
	topK              *int64
	presencePenalty   *float64
	repetitionPenalty *float64
	// answerCap is the card's output length in tokens, 0 when it states none.
	answerCap int64
	// enableThinking, preserveThinking are the chat template's kwargs; a
	// family whose template default is right sends none.
	enableThinking   *bool
	preserveThinking *bool
	// replayReasoning is whether earlier turns' reasoning is sent back.
	replayReasoning *bool
}

func ptrF(v float64) *float64 { return &v }
func ptrI(v int64) *int64     { return &v }
func ptrB(v bool) *bool       { return &v }

// sampled is a profile of temperature and top_p alone.
func sampled(temp, topP float64) *profile {
	return &profile{temperature: ptrF(temp), topP: ptrF(topP)}
}

// replayed marks the profile as one whose template reads earlier turns'
// reasoning, so it is sent back.
func (p *profile) replayed() *profile { p.replayReasoning = ptrB(true); return p }

func has(n string, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// profileRules are tried in order, so a more specific name comes before the
// family it belongs to (Qwen3-Coder-Next before Qwen3-Coder before Qwen3.x,
// Mistral Large and Small before the rest of Mistral). A model matches on its
// lower-cased name without any repository prefix, so a local file's name (a
// quantisation's suffix included) finds its family.
var profileRules = []struct {
	match func(n string) bool
	build func(n string) *profile
}{
	// Nemotron 3 line. Nano v2 and the Llama-Nemotron models are not this
	// line: their switch is a line of the system prompt.
	{func(n string) bool { return has(n, "nemotron-3") && !has(n, "llama") }, nemotron3},
	// Qwen. Coder-Next has no thinking mode, so it sends no switch.
	{func(n string) bool { return has(n, "qwen3-coder-next") }, func(string) *profile {
		return &profile{temperature: ptrF(1.0), topP: ptrF(0.95), topK: ptrI(40), answerCap: 65536}
	}},
	{func(n string) bool { return has(n, "qwen3-coder") }, func(string) *profile {
		return &profile{temperature: ptrF(0.7), topP: ptrF(0.8), topK: ptrI(20), repetitionPenalty: ptrF(1.05), answerCap: 65536}
	}},
	// Qwen3.8 keeps earlier reasoning by default; its card gives a presence
	// penalty of 0 for thinking and 262,144 reasoning plus 131,072 answer
	// tokens.
	{func(n string) bool { return has(n, "qwen3.8", "qwen-3.8") }, func(string) *profile {
		return (&profile{temperature: ptrF(1.0), topP: ptrF(0.95), topK: ptrI(20), presencePenalty: ptrF(0),
			answerCap: 262144 + 131072, enableThinking: ptrB(true)}).replayed()
	}},
	// Qwen3.6's template reads earlier reasoning only when it is preserved.
	{func(n string) bool { return has(n, "qwen3.6", "qwen-3.6") }, func(string) *profile {
		p := qwen35()
		p.preserveThinking = ptrB(true)
		return p.replayed()
	}},
	{func(n string) bool { return has(n, "qwen3.5", "qwen-3.5") }, func(string) *profile { return qwen35() }},
	// DeepSeek V4.1 keeps reasoning when tools are present, as an agent's
	// run has.
	{func(n string) bool { return has(n, "deepseek-v4.1", "deepseek-v4-1") }, func(string) *profile {
		p := sampled(1.0, 1.0)
		p.answerCap = 262144
		return p.replayed()
	}},
	{func(n string) bool { return has(n, "deepseek-r1", "deepseek-v3") }, func(string) *profile { return sampled(0.6, 0.95) }},
	// GLM: the template's own defaults for clear_thinking and the effort
	// kwargs stand.
	{func(n string) bool { return has(n, "glm-4.6", "glm-4.7", "glm-5", "glm4.6", "glm4.7", "glm5") }, func(string) *profile {
		return sampled(1.0, 0.95)
	}},
	// Kimi needs its earlier reasoning_content passed back.
	{func(n string) bool { return has(n, "kimi-k2", "kimi-k3") }, func(string) *profile { return sampled(1.0, 0.95).replayed() }},
	{func(n string) bool { return has(n, "minimax-m2.7") }, func(string) *profile {
		p := sampled(1.0, 0.95)
		p.topK = ptrI(40)
		return p.replayed()
	}},
	{func(n string) bool { return has(n, "minimax-m3") }, func(string) *profile { return sampled(1.0, 0.95).replayed() }},
	// Mistral.
	{func(n string) bool { return has(n, "magistral") }, func(string) *profile {
		p := sampled(0.7, 0.95)
		p.answerCap = 131072
		return p
	}},
	{func(n string) bool { return has(n, "mistral-large-4") }, func(string) *profile { return sampled(1.0, 0.95) }},
	{func(n string) bool { return has(n, "mistral-small-4") }, func(string) *profile {
		return &profile{temperature: ptrF(0.7)}
	}},
	{func(n string) bool { return has(n, "devstral") }, func(string) *profile {
		return &profile{temperature: ptrF(0.15)}
	}},
	// Gemma 4 ships with thinking off, and stays so.
	{func(n string) bool { return has(n, "gemma-4") }, func(string) *profile {
		p := sampled(1.0, 0.95)
		p.topK = ptrI(64)
		return p
	}},
	// Granite's card gives 8,192 as the length of its thinking, not of an
	// answer, so it sets no answer cap.
	{func(n string) bool { return has(n, "granite-4.2") }, func(string) *profile {
		return sampled(1.0, 0.95)
	}},
}

// profileFor is the profile of the family the model name belongs to, or nil
// for a family without one (gpt-oss and Llama 4 among them: their cards state
// no sampling).
func profileFor(name string) *profile {
	n := strings.ToLower(strings.TrimSpace(name))
	if k := strings.LastIndex(n, "/"); k >= 0 {
		n = n[k+1:]
	}
	for _, r := range profileRules {
		if r.match(n) {
			return r.build(n)
		}
	}
	return nil
}

// nemotron3 is the Nemotron 3 and 3.5 line, whose template shares one body.
// Sampling is the card's for reasoning (temperature 1.0, top_p 0.95), and
// the Nano models take 0.6 for tool calls, which an agent's run is. The
// template keeps earlier reasoning only from reasoning_content and puts an
// empty think block where it is missing, so reasoning stays replayed.
func nemotron3(n string) *profile {
	p := &profile{
		temperature:     ptrF(1.0),
		topP:            ptrF(0.95),
		answerCap:       defaultAnswerCap,
		enableThinking:  ptrB(true),
		replayReasoning: ptrB(true),
	}
	switch {
	case has(n, "nano"):
		p.temperature = ptrF(0.6)
		if has(n, "30b") {
			p.answerCap = 10000
		}
	case has(n, "super-vl", "bolt"):
		p.answerCap = 16384
	case has(n, "super", "ultra"):
		// The cards' coding-agent examples cap an answer at 32,000.
		p.answerCap = 32000
	case has(n, "lightning"):
		p.answerCap = 16000
	}
	return p
}

// qwen35 is the Qwen3.5 and 3.6 cards' sampling for thinking, with the
// presence penalty they give against endless repetition.
func qwen35() *profile {
	return &profile{
		temperature:     ptrF(1.0),
		topP:            ptrF(0.95),
		topK:            ptrI(20),
		presencePenalty: ptrF(1.5),
		answerCap:       81920,
		enableThinking:  ptrB(true),
	}
}

// answerLimit is the cap on one answer: the card's length, held to a quarter
// of the model's window where the policy records one. 0 means no cap.
func (p *profile) answerLimit(window int64) int64 {
	limit := p.answerCap
	if limit > 0 && window > 0 {
		limit = min(limit, window/4)
	}
	return max(limit, 0)
}

// modelSettings is the model's sampling, answer cap and reasoning settings:
// the family's profile, each key replaced by the policy's own where the
// person set it. Only a model on an endpoint of the person's own has any; a
// hosted model's harness knows its provider's defaults.
func modelSettings(m policy.Model) (sampling modelSampling, opts providerOptsYAML) {
	if !m.OwnEndpoint() {
		return
	}
	var maxTokens int64
	if p := profileFor(m.Name); p != nil {
		sampling.Temperature, sampling.TopP, sampling.PresencePenalty = p.temperature, p.topP, p.presencePenalty
		opts.TopK, opts.RepetitionPenalty = p.topK, p.repetitionPenalty
		opts.ReplayReasoning = p.replayReasoning
		if p.enableThinking != nil || p.preserveThinking != nil {
			opts.ExtraBody = &extraBodyYAML{ChatTemplateKwargs: &chatTemplateKwargsYAML{
				EnableThinking: p.enableThinking, PreserveThinking: p.preserveThinking,
			}}
		}
		maxTokens = p.answerLimit(m.ContextWindow)
	}
	if s := m.Sampling; s != nil {
		if s.Temperature != nil {
			sampling.Temperature = s.Temperature
		}
		if s.TopP != nil {
			sampling.TopP = s.TopP
		}
		if s.PresencePenalty != nil {
			sampling.PresencePenalty = s.PresencePenalty
		}
		if s.TopK != nil {
			opts.TopK = s.TopK
		}
		if s.MinP != nil {
			opts.MinP = s.MinP
		}
		if s.RepetitionPenalty != nil {
			opts.RepetitionPenalty = s.RepetitionPenalty
		}
	}
	if m.MaxOutputTokens > 0 {
		maxTokens = m.MaxOutputTokens
	}
	sampling.MaxTokens = maxTokens
	return
}

package adapter

import (
	"strings"

	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// ProfilesCheckedOn is the date the values in profileTable were read from
// each family's model card and chat template.
const ProfilesCheckedOn = "2026-11-10"

// defaultAnswerCap is the per-answer token cap for a model whose family card
// states no output length, once the person asks for a cap.
const defaultAnswerCap = 16384

// profile is one open-weight family's reasoning profile: the sampling its
// maker recommends for a model that reasons, the longest answer its card
// shows, and how reasoning is switched on and carried between turns. A nil
// pointer is a value the card does not state, which the server's own default
// then supplies. A family with a profile always carries an answer cap: a
// server given none generates until the model stops, and a reasoning model
// that has fallen into a loop never does.
type profile struct {
	temperature     *float64
	topP            *float64
	topK            *int64
	presencePenalty *float64
	// answerCap is the card's output length in tokens, 0 when it states none.
	answerCap int64
	// enableThinking, preserveThinking are the chat template's kwargs.
	enableThinking   *bool
	preserveThinking *bool
	// replayReasoning is whether earlier turns' reasoning is sent back.
	replayReasoning *bool
}

func ptrF(v float64) *float64 { return &v }
func ptrI(v int64) *int64     { return &v }
func ptrB(v bool) *bool       { return &v }

// profileFor is the profile of the family the model name belongs to, or nil
// for a family without one. The match is on the lower-cased name, without
// any repository prefix, so a local file's name (a quantisation's suffix
// included) finds its family.
func profileFor(name string) *profile {
	n := strings.ToLower(strings.TrimSpace(name))
	if k := strings.LastIndex(n, "/"); k >= 0 {
		n = n[k+1:]
	}
	switch {
	case strings.Contains(n, "nemotron-3") && !strings.Contains(n, "llama"):
		return nemotron3(n)
	case strings.Contains(n, "qwen3.6") || strings.Contains(n, "qwen-3.6"):
		p := qwen35()
		p.preserveThinking = ptrB(true)
		// The template reads earlier reasoning only when it is preserved,
		// so it is sent back.
		p.replayReasoning = ptrB(true)
		return p
	case strings.Contains(n, "qwen3.5") || strings.Contains(n, "qwen-3.5"):
		return qwen35()
	}
	return nil
}

// nemotron3 is the Nemotron 3 and 3.5 line, whose template shares one body.
// Sampling is the card's for reasoning (temperature 1.0, top_p 0.95), and
// the Nano models take 0.6 for tool calls, which an agent's run is. The
// template keeps earlier reasoning only from reasoning_content and puts an
// empty think block where it is missing, so reasoning stays replayed.
// Nano v2 and the Llama-Nemotron models are not this line: their switch is
// a line of the system prompt.
func nemotron3(n string) *profile {
	p := &profile{
		temperature:     ptrF(1.0),
		topP:            ptrF(0.95),
		answerCap:       16384,
		enableThinking:  ptrB(true),
		replayReasoning: ptrB(true),
	}
	switch {
	case strings.Contains(n, "nano"):
		p.temperature = ptrF(0.6)
		p.answerCap = 0
		if strings.Contains(n, "30b") {
			p.answerCap = 10000
		}
	case strings.Contains(n, "super-vl"), strings.Contains(n, "bolt"):
		p.answerCap = 16384
	case strings.Contains(n, "super"), strings.Contains(n, "ultra"):
		// The cards' coding-agent examples cap an answer at 32,000.
		p.answerCap = 32000
	case strings.Contains(n, "lightning"):
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
	if limit <= 0 {
		limit = defaultAnswerCap
	}
	if window > 0 {
		limit = min(limit, window/4)
	}
	return limit
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
		opts.TopK = p.topK
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

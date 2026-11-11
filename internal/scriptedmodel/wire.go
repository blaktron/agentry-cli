package scriptedmodel

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// q is s as a JSON string.
func q(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// callID is the id a call carries: its own, or one made from the request's
// number and the call's place in the reply.
func callID(prefix string, x *exchange, i int, c Call) string {
	if c.ID != "" {
		return c.ID
	}
	return fmt.Sprintf("%s_%d_%d", prefix, x.n, i+1)
}

func anthropicError(status int, message string) string {
	kind := "api_error"
	switch {
	case status == 529:
		kind = "overloaded_error"
	case status == http.StatusTooManyRequests:
		kind = "rate_limit_error"
	case status == http.StatusUnauthorized:
		kind = "authentication_error"
	case status == http.StatusForbidden:
		kind = "permission_error"
	case status == http.StatusRequestEntityTooLarge:
		kind = "request_too_large"
	case status >= 400 && status < 500:
		kind = "invalid_request_error"
	}
	return fmt.Sprintf(`{"type":"error","error":{"type":%s,"message":%s}}`, q(kind), q(message))
}

func chatError(status int, message string) string {
	kind, code := "server_error", "null"
	switch {
	case status == http.StatusTooManyRequests:
		kind, code = "requests", `"rate_limit_exceeded"`
	case status == http.StatusUnauthorized:
		kind, code = "invalid_request_error", `"invalid_api_key"`
	case status == http.StatusPaymentRequired:
		kind, code = "insufficient_quota", `"insufficient_quota"`
	case status >= 400 && status < 500:
		kind = "invalid_request_error"
	}
	return fmt.Sprintf(`{"error":{"message":%s,"type":%s,"param":null,"code":%s}}`, q(message), q(kind), code)
}

// playAnthropic answers as the Anthropic Messages API: a stream of typed
// events when the request asked for one, a whole message otherwise.
func (s Step) playAnthropic(x *exchange) {
	stop := "end_turn"
	switch {
	case s.cut:
		stop = "max_tokens"
	case len(s.calls) > 0:
		stop = "tool_use"
	}
	usage := fmt.Sprintf(`{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}`,
		s.usage.Input, s.usage.CacheWrite, s.usage.CacheRead, s.usage.Output)
	id := fmt.Sprintf("msg_%d", x.n)
	signature := fmt.Sprintf("signature-%d", x.n)

	if !x.req.Stream && s.kind == kindReply {
		var blocks []string
		if s.reasoning != "" {
			blocks = append(blocks, fmt.Sprintf(`{"type":"thinking","thinking":%s,"signature":%s}`, q(s.reasoning), q(signature)))
		}
		if s.text != "" {
			blocks = append(blocks, fmt.Sprintf(`{"type":"text","text":%s}`, q(s.text)))
		}
		for i, c := range s.calls {
			if s.cut {
				// A whole message cannot carry half a call: the
				// server ends the message before it.
				continue
			}
			blocks = append(blocks, fmt.Sprintf(`{"type":"tool_use","id":%s,"name":%s,"input":%s}`, q(callID("toolu", x, i, c)), q(c.Name), c.Arguments))
		}
		x.json(http.StatusOK, fmt.Sprintf(`{"id":%s,"type":"message","role":"assistant","model":%s,"content":[%s],"stop_reason":%s,"stop_sequence":null,"usage":%s}`,
			q(id), q(x.req.Model), join(blocks), q(stop), usage))
		return
	}

	x.sse()
	x.event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%s,"type":"message","role":"assistant","model":%s,"content":[],"stop_reason":null,"stop_sequence":null,"usage":%s}}`,
		q(id), q(x.req.Model), fmt.Sprintf(`{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":0}`, s.usage.Input, s.usage.CacheWrite, s.usage.CacheRead)))

	index := 0
	block := func(start string, deltas ...string) {
		x.event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, index, start))
		for _, d := range deltas {
			x.event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, index, d))
		}
	}
	end := func() {
		x.event("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index))
		index++
	}

	if s.reasoning != "" {
		a, b := halves(s.reasoning)
		block(`{"type":"thinking","thinking":"","signature":""}`,
			`{"type":"thinking_delta","thinking":`+q(a)+`}`,
			`{"type":"thinking_delta","thinking":`+q(b)+`}`,
			`{"type":"signature_delta","signature":`+q(signature)+`}`)
		end()
	}
	if s.text != "" {
		a, b := halves(s.text)
		block(`{"type":"text","text":""}`, `{"type":"text_delta","text":`+q(a)+`}`)
		if s.kind == kindStall {
			x.hold()
			return
		}
		if s.kind == kindDrop {
			x.drop()
			return
		}
		x.event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, q(b)))
		end()
	} else if s.kind == kindStall {
		x.hold()
		return
	} else if s.kind == kindDrop {
		x.drop()
		return
	}
	for i, c := range s.calls {
		start := fmt.Sprintf(`{"type":"tool_use","id":%s,"name":%s,"input":{}}`, q(callID("toolu", x, i, c)), q(c.Name))
		a, b := halves(c.Arguments)
		block(start, `{"type":"input_json_delta","partial_json":`+q(a)+`}`, `{"type":"input_json_delta","partial_json":`+q(b)+`}`)
		end()
	}
	x.event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%s,"stop_sequence":null},"usage":{"output_tokens":%d}}`, q(stop), s.usage.Output))
	x.event("message_stop", `{"type":"message_stop"}`)
}

// playChat answers as Chat Completions: a stream of chunks ended by [DONE]
// when the request asked for one, a whole completion otherwise. A stream
// carries usage only when the request set stream_options.include_usage, as
// OpenAI's does.
func (s Step) playChat(x *exchange) {
	finish := "stop"
	switch {
	case s.cut:
		finish = "length"
	case len(s.calls) > 0:
		finish = "tool_calls"
	}
	usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d},"completion_tokens_details":{"reasoning_tokens":%d}}`,
		s.usage.Input+s.usage.CacheRead, s.usage.Output, s.usage.Input+s.usage.CacheRead+s.usage.Output, s.usage.CacheRead, s.usage.Reasoning)
	id := fmt.Sprintf("chatcmpl-%d", x.n)

	if !x.req.Stream && s.kind == kindReply {
		msg := `"role":"assistant","content":` + q(s.text)
		if s.reasoning != "" {
			msg += `,"reasoning_content":` + q(s.reasoning)
		}
		var calls []string
		for i, c := range s.calls {
			calls = append(calls, fmt.Sprintf(`{"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}`, q(callID("call", x, i, c)), q(c.Name), q(c.Arguments)))
		}
		if len(calls) > 0 {
			msg += `,"tool_calls":[` + join(calls) + `]`
		}
		x.json(http.StatusOK, fmt.Sprintf(`{"id":%s,"object":"chat.completion","created":0,"model":%s,"choices":[{"index":0,"message":{%s},"finish_reason":%s}],"usage":%s}`,
			q(id), q(x.req.Model), msg, q(finish), usage))
		return
	}

	chunk := func(delta, finishReason string) {
		x.event("", fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":0,"model":%s,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`,
			q(id), q(x.req.Model), delta, finishReason))
	}

	x.sse()
	chunk(`{"role":"assistant","content":""}`, "null")
	if s.reasoning != "" {
		a, b := halves(s.reasoning)
		chunk(`{"reasoning_content":`+q(a)+`}`, "null")
		chunk(`{"reasoning_content":`+q(b)+`}`, "null")
	}
	if s.text != "" {
		a, b := halves(s.text)
		chunk(`{"content":`+q(a)+`}`, "null")
		if s.kind == kindReply {
			chunk(`{"content":`+q(b)+`}`, "null")
		}
	}
	if s.kind == kindStall {
		x.hold()
		return
	}
	if s.kind == kindDrop {
		x.drop()
		return
	}
	for i, c := range s.calls {
		a, b := halves(c.Arguments)
		chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]}`, i, q(callID("call", x, i, c)), q(c.Name)), "null")
		chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}`, i, q(a)), "null")
		chunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}`, i, q(b)), "null")
	}
	chunk(`{}`, q(finish))
	if x.req.IncludeUsage {
		x.event("", fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":0,"model":%s,"choices":[],"usage":%s}`, q(id), q(x.req.Model), usage))
	}
	x.event("", "[DONE]")
}

func join(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

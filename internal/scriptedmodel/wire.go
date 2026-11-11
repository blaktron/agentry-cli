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

// playResponses answers as the OpenAI Responses API: a stream of typed
// events ended by response.completed, or by response.incomplete when the
// reply stopped at the output limit; a whole response when the request
// asked for no stream. Reasoning is a reasoning item with a summary and an
// encrypted_content a client sends back to continue without stored state.
func (s Step) playResponses(x *exchange) {
	id := fmt.Sprintf("resp_%d", x.n)
	usage := fmt.Sprintf(`{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d},"output_tokens":%d,"output_tokens_details":{"reasoning_tokens":%d},"total_tokens":%d}`,
		s.usage.Input+s.usage.CacheRead, s.usage.CacheRead, s.usage.Output, s.usage.Reasoning, s.usage.Input+s.usage.CacheRead+s.usage.Output)

	var items []string
	reasoningItem := fmt.Sprintf(`{"type":"reasoning","id":"rs_%d","summary":[{"type":"summary_text","text":%s}],"encrypted_content":"encrypted-%d"}`, x.n, q(s.reasoning), x.n)
	messageItem := fmt.Sprintf(`{"type":"message","id":"msg_%d","role":"assistant","status":"completed","content":[{"type":"output_text","text":%s,"annotations":[]}]}`, x.n, q(s.text))
	callItem := func(i int, c Call) string {
		return fmt.Sprintf(`{"type":"function_call","id":"fc_%d_%d","call_id":%s,"name":%s,"arguments":%s,"status":"completed"}`, x.n, i+1, q(callID("call", x, i, c)), q(c.Name), q(c.Arguments))
	}
	if s.reasoning != "" {
		items = append(items, reasoningItem)
	}
	if s.text != "" {
		items = append(items, messageItem)
	}
	if !s.cut {
		for i, c := range s.calls {
			items = append(items, callItem(i, c))
		}
	}
	status, details := "completed", "null"
	if s.cut {
		status, details = "incomplete", `{"reason":"max_output_tokens"}`
	}
	response := fmt.Sprintf(`{"id":%s,"object":"response","model":%s,"status":%s,"incomplete_details":%s,"output":[%s],"usage":%s}`,
		q(id), q(x.req.Model), q(status), details, join(items), usage)

	if !x.req.Stream && s.kind == kindReply {
		x.json(http.StatusOK, response)
		return
	}

	x.sse()
	send := func(typ, rest string) {
		x.event(typ, fmt.Sprintf(`{"type":%s,%s}`, q(typ), rest))
	}
	send("response.created", fmt.Sprintf(`"response":{"id":%s,"object":"response","model":%s,"status":"in_progress","output":[]}`, q(id), q(x.req.Model)))
	index := 0
	if s.reasoning != "" {
		a, b := halves(s.reasoning)
		send("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"type":"reasoning","id":"rs_%d","summary":[]}`, index, x.n))
		send("response.reasoning_summary_text.delta", fmt.Sprintf(`"output_index":%d,"summary_index":0,"delta":%s`, index, q(a)))
		send("response.reasoning_summary_text.delta", fmt.Sprintf(`"output_index":%d,"summary_index":0,"delta":%s`, index, q(b)))
		send("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":%s`, index, reasoningItem))
		index++
	}
	if s.text != "" {
		a, b := halves(s.text)
		send("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"type":"message","id":"msg_%d","role":"assistant","status":"in_progress","content":[]}`, index, x.n))
		send("response.output_text.delta", fmt.Sprintf(`"output_index":%d,"content_index":0,"delta":%s`, index, q(a)))
		if s.kind == kindStall {
			x.hold()
			return
		}
		if s.kind == kindDrop {
			x.drop()
			return
		}
		send("response.output_text.delta", fmt.Sprintf(`"output_index":%d,"content_index":0,"delta":%s`, index, q(b)))
		send("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":%s`, index, messageItem))
		index++
	} else if s.kind == kindStall {
		x.hold()
		return
	} else if s.kind == kindDrop {
		x.drop()
		return
	}
	for i, c := range s.calls {
		a, b := halves(c.Arguments)
		send("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"type":"function_call","id":"fc_%d_%d","call_id":%s,"name":%s,"arguments":"","status":"in_progress"}`, index, x.n, i+1, q(callID("call", x, i, c)), q(c.Name)))
		send("response.function_call_arguments.delta", fmt.Sprintf(`"output_index":%d,"delta":%s`, index, q(a)))
		if s.cut {
			// The reply stopped inside this call: it is never done.
			break
		}
		send("response.function_call_arguments.delta", fmt.Sprintf(`"output_index":%d,"delta":%s`, index, q(b)))
		send("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":%s`, index, callItem(i, c)))
		index++
	}
	if s.cut {
		send("response.incomplete", `"response":`+response)
		return
	}
	send("response.completed", `"response":`+response)
}

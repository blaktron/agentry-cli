package adapter

import "testing"

// The passing model failures a harness ends on are named; a failure of the
// request itself, of the key or of the model's name is not one.
func TestTransientModelFailures(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{`Error: model failed: POST "http://192.168.1.20:8080/v1/chat/completions": 502 Bad Gateway`, true},
		{`error, status code: 500, status: 500 Internal Server Error, message: upstream failed`, true},
		{`{"error":{"code":503,"message":"Loading model"}}`, true},
		{`Error: model failed: POST "https://api.example/v1/chat/completions": 429 Too Many Requests`, true},
		{`rate_limit_exceeded: please slow down`, true},
		{`read tcp 172.18.0.5:41234->192.168.1.20:8080: read: connection reset by peer`, true},
		{`dial tcp 192.168.1.20:8080: connect: connection refused`, true},
		{`error receiving from stream: unexpected EOF`, true},
		{`The connection to the model was closed unexpectedly before it completed its response.`, true},
		{`Error: model stream stalled: no data received from upstream`, true},
		{`Error: The conversation has exceeded the model's context window. Compact the conversation to reduce its size, or start a new session.`, true},
		{`HTTP 400: request (40961 tokens) exceeds the available context size (32768 tokens), try increasing it`, true},
		{`529 Overloaded {"type":"error","error":{"type":"overloaded_error"}}`, true},
		{`Error: model failed: 400 Bad Request {"error":{"message":"invalid tool schema"}}`, false},
		{`Error: model failed: 401 Unauthorized: invalid api key`, false},
		{`Error: model failed: 404 Not Found: model "qwen9" does not exist`, false},
		{`Error: parsing config file: unknown field "foo"`, false},
		{`the prompt was 429 tokens long; the model answered 512 tokens`, false},
		{`{"error":{"code":429,"message":"slow down"}}`, true},
		{"", false},
	}
	for _, c := range cases {
		reason, ok := TransientModelFailure(c.text)
		if ok != c.want {
			t.Errorf("%q: transient %v, want %v", c.text, ok, c.want)
		}
		if ok && reason == "" {
			t.Errorf("%q: no reason named", c.text)
		}
	}
}

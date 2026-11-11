package llm

import "testing"

func TestStatedWindowReadsEachServersRefusal(t *testing.T) {
	cases := map[string]int64{
		"This model's maximum context length is 32768 tokens. However, you requested 40961 tokens.":                                         32768,
		"This model's maximum context length is 128,000 tokens. However, your messages resulted in 130,512 tokens.":                         128000,
		"request (12009 tokens) exceeds the available context size (2048 tokens), try increasing it":                                        2048,
		`{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error","n_ctx":4096}}`: 4096,
		"The number of tokens to keep from the initial prompt is greater than the context length (n_keep: 5746 >= n_ctx: 4096).":            4096,
		"prompt is too long: 213462 tokens > 200000 maximum":                                                                                200000,
		"The input token count (1250001) exceeds the maximum number of tokens allowed (1048576).":                                           1048576,
		"the request exceeds the available context size, try increasing it":                                                                 0,
		"maximum context length is 999999999999 tokens":                                                                                     0,
		"maximum context length is 12 tokens":                                                                                               0,
		"prompt too long; exceeded max context length by 1205 tokens":                                                                       0,
	}
	for message, want := range cases {
		if got := StatedWindow(message); got != want {
			t.Errorf("%q: %d, want %d", message, got, want)
		}
	}
}

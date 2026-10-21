package adapter

import "testing"

// A provider's capacity answer is recognized in each vendor's words; a rate
// limit, a plan's limit, credits and other failures are not one.
func TestAtCapacityKnowsTheProvidersWords(t *testing.T) {
	for _, text := range []string{
		"codex session: turn failed: Selected model is at capacity. Please try a different model.",
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		"API Error: 529 Overloaded",
		`POST "https://api.openai.com/v1/responses": 503 Service Unavailable {"error":{"code":"server_is_overloaded"}}`,
		`{"error":{"code":503,"message":"The model is overloaded. Please try again later.","status":"UNAVAILABLE"}}`,
		`{"error":{"status": 529}}`,
	} {
		if !AtCapacity(text) {
			t.Errorf("not recognized as capacity: %s", text)
		}
	}
	for _, text := range []string{
		"exceeded retry limit, last status: 429 Too Many Requests",
		"You've hit your usage limit. Try again at 3:00 PM.",
		"402 Payment Required: insufficient_credits",
		"model failed: the provider is down",
		"the tool's output was 503 lines long",
		"",
	} {
		if AtCapacity(text) {
			t.Errorf("taken for capacity: %s", text)
		}
	}
}

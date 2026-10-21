package adapter

import "regexp"

// atCapacityRE matches a model provider's answer that the model cannot take
// the request right now for want of capacity: OpenAI's "Selected model is at
// capacity" and its server_is_overloaded code, Anthropic's overloaded_error
// (HTTP 529) and the "Overloaded" its SDKs quote, a "503 Service Unavailable",
// a body's 503 or 529 code or status, and Google's UNAVAILABLE status. A 429
// is not one: it is a rate limit, and on a model sign-in the plan's limit,
// which is never waited out.
var atCapacityRE = regexp.MustCompile(`(?i)\bat capacity\b|\bserver_is_overloaded\b|\boverloaded(?:_error)?\b|\b503 service unavailable\b|"(?:code|status)"\s*:\s*"?(?:503|529)\b|"status"\s*:\s*"UNAVAILABLE"`)

// AtCapacity reports whether a harness's error is a model provider saying
// the model is at capacity: a refusal of the moment, not of the run, which
// the engine waits out and sends again.
func AtCapacity(text string) bool { return atCapacityRE.MatchString(text) }

package adapter

import "regexp"

// transientFailures are the model failures a harness can end on that belong
// to the moment rather than the run: a server error or a rate limit after the
// harness's own retries, a connection that was refused, reset or dropped, a
// stream that stalled, and an overflow the harness could not compact. Each
// names the failure for the notice that announces the wait. Checked in
// order: an overflow's text can also quote the 400 it came with.
var transientFailures = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)exceeded the model's context window|\bcontext window overflow\b|exceeds the available context size|maximum context length|is greater than the context length`),
		"the conversation outgrew the model's window"},
	{regexp.MustCompile(`(?i)\bmodel stream stalled\b|\bno data received from upstream\b`),
		"the model's stream stalled"},
	{regexp.MustCompile(`(?i)\bconnection (?:refused|reset)\b|\bbroken pipe\b|\bunexpected EOF\b|\bno route to host\b|\bi/o timeout\b|connection to the model was closed unexpectedly|\bserver closed idle connection\b`),
		"the connection to the model server was lost"},
	{regexp.MustCompile(`(?i)\b(?:HTTP|status(?: code)?:?)\s*429\b|\b429 Too Many Requests\b|"(?:code|status)"\s*:\s*"?429\b|\btoo many requests\b|\brate[ _-]?limit`),
		"the model server limited the rate of requests"},
	{regexp.MustCompile(`(?i)\b(?:HTTP|status(?: code)?:?)\s*5\d\d\b|\b5\d\d (?:Internal Server Error|Bad Gateway|Service Unavailable|Gateway Timeout)\b|"(?:code|status)"\s*:\s*"?5\d\d\b`),
		"the model server answered with an error"},
}

// TransientModelFailure reports whether a harness's error is a model failure
// of the moment (transientFailures), and names it. A capacity answer is one
// too (AtCapacity). On a model sign-in a 429 is the plan's limit, which the
// engine reads from the harness's events before it looks here.
func TransientModelFailure(text string) (reason string, ok bool) {
	if text == "" {
		return "", false
	}
	if AtCapacity(text) {
		return "the model's provider said the model is at capacity", true
	}
	for _, f := range transientFailures {
		if f.re.MatchString(text) {
			return f.reason, true
		}
	}
	return "", false
}

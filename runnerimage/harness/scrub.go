package harness

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// scrubbed stands where a secret-shaped value was.
const scrubbed = "[secret removed]"

// secretShapes are the forms a credential is recognised by: each a prefix or
// a structure its issuer gives it, never a guess from how random a string
// looks. A pattern with a group keeps what is outside the group.
var secretShapes = []*regexp.Regexp{
	// A private key in PEM armour, whatever kind.
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`),
	// AWS access key ids.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// GitHub tokens: personal, OAuth, user-to-server, server-to-server,
	// refresh, and fine-grained personal tokens.
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,255}\b`),
	// GitLab personal tokens.
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),
	// Model providers' keys that start "sk-" (Anthropic, OpenAI, OpenRouter
	// and others that follow the form).
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	// Payment processors' live keys, secret and restricted.
	regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{20,}`),
	// Slack tokens.
	regexp.MustCompile(`\bxox[abeoprs]-[A-Za-z0-9-]{10,}`),
	// Google API keys.
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	// Hugging Face and npm tokens.
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`),
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`),
	// A signed web token: three base64url parts, the first two JSON objects.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	// The credential of an Authorization header or its like.
	regexp.MustCompile(`(?i)\bauthorization["']?\s*[:=]\s*["']?(?:bearer|basic|token)\s+([A-Za-z0-9._~+/=-]{16,})`),
	// The password of an address that carries one.
	regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://[^/\s:@"']+:([^/\s@"']{3,})@`),
}

// scrubText is s with every secret-shaped value replaced.
func scrubText(s string) string {
	for _, re := range secretShapes {
		if re.NumSubexp() == 0 {
			s = re.ReplaceAllString(s, scrubbed)
			continue
		}
		s = replaceGroup(re, s)
	}
	return s
}

// replaceGroup replaces what re's one group matched and keeps the rest of
// each match.
func replaceGroup(re *regexp.Regexp, s string) string {
	locs := re.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range locs {
		if m[2] < 0 {
			continue
		}
		b.WriteString(s[last:m[2]])
		b.WriteString(scrubbed)
		last = m[3]
	}
	b.WriteString(s[last:])
	return b.String()
}

// scrubArguments is a call's arguments with every secret-shaped value in a
// string replaced. Arguments nothing was found in are returned as they
// were, byte for byte.
func scrubArguments(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw
	}
	changed := false
	v = scrubValue(v, &changed)
	if !changed {
		return raw
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return raw
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

func scrubValue(v any, changed *bool) any {
	switch t := v.(type) {
	case string:
		if s := scrubText(t); s != t {
			*changed = true
			return s
		}
	case []any:
		for i := range t {
			t[i] = scrubValue(t[i], changed)
		}
	case map[string]any:
		// A member's name is the model's text as much as its value is.
		out := make(map[string]any, len(t))
		for k, member := range t {
			name := scrubText(k)
			*changed = *changed || name != k
			out[name] = scrubValue(member, changed)
		}
		return out
	}
	return v
}

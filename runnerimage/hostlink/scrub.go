package hostlink

import (
	"encoding/base64"
	"slices"
	"strings"
)

// The scrub (plan runtime-hosts.md D9, D19): what a target host prints
// reaches the agent unchanged, keys and tokens read there included, since
// the agent is there to manage the machine. The one thing taken out is the
// run's own credentials for its target hosts (a key, its passphrase, a
// password, a sudo password), which the person gave the runner and never
// the agent: a command can print them back (a password sudo was fed and
// read again, say), and they are masked in every spelling the host link
// can know, raw and base64.

// Redacted is what a run credential is replaced by.
const Redacted = "[redacted: a sign-in of this run's target host]"

// minScrub is the shortest value the scrub masks: shorter, and it would
// mask ordinary text (a password that short is the person's to change).
const minScrub = 6

type scrubber struct{ values []string }

// newScrubber masks each credential, its trimmed form, its base64
// spellings, and for a private key each line of its body, longest first.
func newScrubber(creds ...string) *scrubber {
	var vals []string
	add := func(v string) {
		if len(v) >= minScrub && !slices.Contains(vals, v) {
			vals = append(vals, v)
		}
	}
	for _, c := range creds {
		for _, v := range []string{c, strings.TrimSpace(c)} {
			add(v)
			add(base64.StdEncoding.EncodeToString([]byte(v)))
			add(base64.RawStdEncoding.EncodeToString([]byte(v)))
			add(base64.URLEncoding.EncodeToString([]byte(v)))
		}
		if strings.Contains(c, "PRIVATE KEY-----") {
			var body []string
			for _, line := range strings.Split(c, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "-----") {
					continue
				}
				body = append(body, line)
				if len(line) >= 16 {
					add(line)
				}
			}
			add(strings.Join(body, ""))
		}
	}
	slices.SortFunc(vals, func(a, b string) int { return len(b) - len(a) })
	return &scrubber{values: vals}
}

func (s *scrubber) String(t string) string {
	if s == nil {
		return t
	}
	for _, v := range s.values {
		if strings.Contains(t, v) {
			t = strings.ReplaceAll(t, v, Redacted)
		}
	}
	return t
}

// Holds says whether t carries a credential in any spelling the scrub
// knows.
func (s *scrubber) Holds(t string) bool {
	if s == nil {
		return false
	}
	for _, v := range s.values {
		if strings.Contains(t, v) {
			return true
		}
	}
	return false
}

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

// MinScrub is the shortest value the scrub masks: shorter, and it would
// mask ordinary text (a password that short is the person's to change; the
// receipt says when one is).
const MinScrub = 6

type scrubber struct{ values []string }

// Spellings are the ways a credential can appear in what a command
// prints: as it is, trimmed, in base64 (with and without the line end a
// command adds, standard, unpadded and URL-safe), and for a private key
// each line of its body and the body whole. The host link's scrub and the
// engine's redactor take the same list, so the two cannot drift.
func Spellings(c string) []string {
	var out []string
	add := func(v string) {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	t := strings.TrimSpace(c)
	add(c)
	add(t)
	for _, v := range []string{c, t, t + "\n", t + "\r\n"} {
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
	return out
}

// newScrubber masks every spelling of each credential, longest first.
func newScrubber(creds ...string) *scrubber {
	var vals []string
	for _, c := range creds {
		for _, v := range Spellings(c) {
			if len(v) >= MinScrub && !slices.Contains(vals, v) {
				vals = append(vals, v)
			}
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

// heldTail is how many bytes at b's end could begin a credential that the
// bytes after them would finish: the longest end of b that is the head of
// a value the scrub masks, and shorter than it. A shell session's output
// arrives in pieces, so a read keeps that much back for the next, and no
// read ever returns half of a sign-in that neither half shows.
func (s *scrubber) heldTail(b []byte) int {
	if s == nil {
		return 0
	}
	held := 0
	for _, v := range s.values {
		for k := min(len(v)-1, len(b)); k > held; k-- {
			if string(b[len(b)-k:]) == v[:k] {
				held = k
				break
			}
		}
	}
	return held
}

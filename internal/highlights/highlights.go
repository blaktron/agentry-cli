// Package highlights is what each release of the agentry CLI brings, in a
// few plain lines for the person using it. The release's notes and the
// download page carry them; the CLI itself prints nothing of them.
//
// highlights.json is compiled in so that its test can hold every entry to
// the rules below on every build.
package highlights

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// MaxLines is how many lines one version may carry, and MaxLineLength how
// long each may be, in characters.
const (
	MaxLines      = 4
	MaxLineLength = 120
)

// Entry is one version's highlights. Version is X.Y.Z: every dev
// prerelease of a version shares its entry.
type Entry struct {
	Version    string   `json:"version"`
	Highlights []string `json:"highlights"`
}

type file struct {
	Versions []Entry `json:"versions"`
}

//go:embed highlights.json
var embedded []byte

var load = sync.OnceValues(func() ([]Entry, error) { return Parse(embedded) })

// All is every version's entry, newest first. An error means the compiled
// file breaks its own rules, which its test refuses before a build.
func All() ([]Entry, error) { return load() }

var versionRE = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)

// Parse reads a highlights file and checks every rule: versions are X.Y.Z,
// newest first, each once, with one to MaxLines lines that CheckLine accepts.
func Parse(data []byte) ([]Entry, error) {
	var f file
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("highlights: %w", err)
	}
	if dec.More() {
		return nil, errors.New("highlights: more than one JSON value")
	}
	for i, e := range f.Versions {
		if !versionRE.MatchString(e.Version) {
			return nil, fmt.Errorf("highlights: %q is not a version of the form X.Y.Z", e.Version)
		}
		if i > 0 && compare(f.Versions[i-1].Version, e.Version) <= 0 {
			return nil, fmt.Errorf("highlights: %s follows %s: versions go newest first, each once", e.Version, f.Versions[i-1].Version)
		}
		if n := len(e.Highlights); n == 0 || n > MaxLines {
			return nil, fmt.Errorf("highlights: %s has %d lines, and a version has one to %d", e.Version, n, MaxLines)
		}
		for _, line := range e.Highlights {
			if err := CheckLine(line); err != nil {
				return nil, fmt.Errorf("highlights: %s: %w", e.Version, err)
			}
		}
	}
	return f.Versions, nil
}

var (
	// Markdown or HTML: the lines are shown as text, never rendered.
	markupRE = regexp.MustCompile("[*_`#<>\\[\\]|~]|^\\s*[-+] |^\\s*\\d+[.)] ")
	// A commit subject's prefix, "feat:" or "fix(run card):".
	prefixRE = regexp.MustCompile(`(?i)^(feat|fix|chore|docs|refactor|perf|test|tests|build|ci|style|revert|deps)(\([^)]*\))?!?:`)
	// A register id, written for the people who build the CLI.
	registerRE = regexp.MustCompile(`\b(DEV|DR|D)-\d+\b`)
	// An address or a host name. The product names its places in words.
	hostRE = regexp.MustCompile(`(?i)\bhttps?://|\b[a-z0-9-]+(\.[a-z0-9-]+)*\.(net|com|org|io|dev|ai|app|internal|local|lan)\b|\bagentry-[a-z0-9-]+`)
)

// CheckLine says why a line cannot be a highlight, or nil. A line is one
// plain sentence for someone using the CLI, ending in a full stop, a
// question mark or an exclamation mark.
func CheckLine(line string) error {
	switch {
	case strings.TrimSpace(line) != line || line == "":
		return fmt.Errorf("%q: a line has no leading or trailing space and is not empty", line)
	case utf8.RuneCountInString(line) > MaxLineLength:
		return fmt.Errorf("%q: %d characters, and a line has at most %d", line, utf8.RuneCountInString(line), MaxLineLength)
	case strings.ContainsAny(line, "\n\r\t"):
		return fmt.Errorf("%q: a line is one line", line)
	case prefixRE.MatchString(line):
		return fmt.Errorf("%q: a commit prefix; say what the person can now do", line)
	case markupRE.MatchString(line):
		return fmt.Errorf("%q: Markdown, HTML or a reference; a line is plain text", line)
	case registerRE.MatchString(line):
		return fmt.Errorf("%q: a register id; say what the person can now do", line)
	case hostRE.MatchString(line):
		return fmt.Errorf("%q: an address or a host name; name the place in words", line)
	case !strings.ContainsAny(line[len(line)-1:], ".?!"):
		return fmt.Errorf("%q: a line is a sentence, ending in a full stop", line)
	}
	return nil
}

// compare orders two X.Y.Z versions Parse has already matched.
func compare(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range pa {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return 0
}

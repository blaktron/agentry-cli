// How to run a QuickAgent, as `agentry info` shows it: the latest
// release's intake questions and how to answer each, the files it takes,
// the keys it declares, its modes and its publisher's guide. A person reads
// it before `agentry run`; a host agent reads the same in --json before it
// answers the questions with --intake. Everything comes from the manifest
// the resolve already returned. What the publisher wrote is cut to a bound
// and, on a terminal, cleaned and set behind termtext's gutter, so it can
// neither rewrite the terminal nor pass for the CLI's own lines.
package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/intake"
	"github.com/blaktron/agentry-cli/pkg/termtext"
)

const (
	// maxInfoOverview bounds the guide's overview info shows, and
	// maxInfoLine each of its other lines and each question's prompt.
	maxInfoOverview = 2 << 10
	maxInfoLine     = 400
)

// infoRun is how to run a release: the "run" object of info --json.
type infoRun struct {
	Intake    []infoQuestion `json:"intake"`
	Takes     []string       `json:"takes"`
	InputView string         `json:"inputView"`
	Secrets   []infoSecret   `json:"secrets"`
	Modes     []string       `json:"modes"`
	Guide     *infoGuide     `json:"guide,omitempty"`
}

// infoQuestion is one intake question and how to answer it.
type infoQuestion struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Required bool     `json:"required"`
	Prompt   string   `json:"prompt"`
	Choices  []string `json:"choices,omitempty"`
	// PersonsOwn marks a question only the person running it answers: a
	// secret, or a target host they set up.
	PersonsOwn bool `json:"personsOwn"`
	// Answer says how to answer it, in a line.
	Answer string `json:"answer"`
}

// infoSecret is one declared key.
type infoSecret struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional"`
}

// infoGuide is the publisher's guide, cut to its bounds.
type infoGuide struct {
	Overview string   `json:"overview,omitempty"`
	Prepare  []string `json:"prepare,omitempty"`
	Files    string   `json:"files,omitempty"`
	Task     string   `json:"task,omitempty"`
}

// howToRun reads how to run m from its manifest.
func howToRun(m *manifest.Manifest) *infoRun {
	r := &infoRun{
		Intake:    []infoQuestion{},
		Secrets:   []infoSecret{},
		InputView: string(m.DefaultInputView()),
	}
	var sites []string
	if b := m.Capabilities.Browser; b != nil {
		sites = b.Sites
	}
	carried := map[string]bool{}
	for _, q := range m.Intake {
		t := q.Type
		if t == "" {
			t = manifest.IntakeText
		}
		if t == manifest.IntakeSecret {
			carried[intake.SecretName(q.ID)] = true
		}
		r.Intake = append(r.Intake, infoQuestion{
			ID:         q.ID,
			Type:       string(t),
			Required:   q.Required,
			Prompt:     cutText(strings.TrimSpace(q.Prompt), maxInfoLine),
			Choices:    q.Choices,
			PersonsOwn: t == manifest.IntakeSecret || t == manifest.IntakeHost,
			Answer:     answerLine(q, t, slices.Contains(sites, q.ID)),
		})
	}
	for _, k := range m.TakesKinds() {
		r.Takes = append(r.Takes, string(k))
	}
	names := slices.Clone(m.Capabilities.Secrets)
	for _, s := range m.Capabilities.MCPServers {
		names = append(names, s.Secrets...)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] || carried[n] {
			continue
		}
		seen[n] = true
		r.Secrets = append(r.Secrets, infoSecret{Name: n, Optional: m.Capabilities.IsOptionalSecret(n)})
	}
	for _, mo := range m.Modes {
		r.Modes = append(r.Modes, string(mo))
	}
	if g := m.Guide; g != nil {
		ig := &infoGuide{
			Overview: cutText(strings.TrimSpace(g.Overview), maxInfoOverview),
			Files:    cutText(strings.TrimSpace(g.Files), maxInfoLine),
			Task:     cutText(strings.TrimSpace(g.Task), maxInfoLine),
		}
		for _, p := range g.Prepare {
			if p = strings.TrimSpace(p); p != "" {
				ig.Prepare = append(ig.Prepare, cutText(p, maxInfoLine))
			}
		}
		if ig.Overview != "" || len(ig.Prepare) > 0 || ig.Files != "" || ig.Task != "" {
			r.Guide = ig
		}
	}
	return r
}

// answerLine says, in a line, how to answer q of type t; site marks a url
// question the release's browser opens as a site.
func answerLine(q manifest.IntakeQuestion, t manifest.IntakeType, site bool) string {
	switch {
	case t == manifest.IntakeSecret:
		return "The person's own value, typed on their terminal when the run asks; never give it for them."
	case t == manifest.IntakeHost:
		return "The person's own target host, [user@]host[:port], which they set up and sign in to; never give it for them."
	case t == manifest.IntakeRepository:
		return "The repository's https address; add #branch, #tag or #commit to pick one. Agentry fetches it for the run: nothing needs downloading."
	case site:
		return "The site's http or https address, which the run's browser opens."
	case t == manifest.IntakeURL:
		return "One file's https address. Agentry fetches it for the run."
	case t == manifest.IntakeChoice:
		return "One of " + strings.Join(quoteEach(q.Choices), ", ") + ", as written."
	case t == manifest.IntakeFile:
		return "A path to a file or folder on this computer."
	}
	return "The text it asks for."
}

// renderHowToRun prints r under info's "how to run it" heading.
func renderHowToRun(out io.Writer, m *manifest.Manifest, r *infoRun) {
	_, _ = fmt.Fprintln(out, "  how to run it:")
	if len(r.Intake) == 0 {
		_, _ = fmt.Fprintln(out, "    questions: none")
	} else {
		_, _ = fmt.Fprintln(out, "    questions (answer each with --intake id=value):")
		for _, q := range r.Intake {
			need := "optional"
			if q.Required {
				need = "required"
			}
			_, _ = fmt.Fprintf(out, "      %s (%s, %s): %s\n", q.ID, q.Type, need, termtext.Safe(q.Prompt))
			_, _ = fmt.Fprintf(out, "        answer: %s\n", termtext.Safe(q.Answer))
		}
	}
	files := inputsLine(m)
	if files == "" {
		files = "Files it takes: any kind; read as text or as uploaded."
	}
	_, _ = fmt.Fprintf(out, "    %s Give them with --file.\n", files)
	if len(r.Secrets) > 0 {
		parts := make([]string, len(r.Secrets))
		for i, s := range r.Secrets {
			parts[i] = s.Name
			if s.Optional {
				parts[i] += " (optional)"
			}
		}
		_, _ = fmt.Fprintf(out, "    keys: %s\n", strings.Join(parts, ", "))
	}
	if !slices.Contains(r.Modes, string(manifest.ModeAutonomous)) {
		_, _ = fmt.Fprintln(out, "    it runs only as an interactive session")
	}
	g := r.Guide
	if g == nil {
		return
	}
	_, _ = fmt.Fprintln(out, "  guide (the publisher's words):")
	if g.Overview != "" {
		_, _ = fmt.Fprintf(out, "%s\n", termtext.Quote(g.Overview))
	}
	for _, p := range g.Prepare {
		_, _ = fmt.Fprintf(out, "    have ready: %s\n", termtext.Safe(p))
	}
	if g.Files != "" {
		_, _ = fmt.Fprintf(out, "    files: %s\n", termtext.Safe(g.Files))
	}
	if g.Task != "" {
		_, _ = fmt.Fprintf(out, "    task: %s\n", termtext.Safe(g.Task))
	}
}

// cutText is s cut to max runes, marked with an ellipsis when cut.
func cutText(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// quoteEach is each of ss in double quotes.
func quoteEach(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

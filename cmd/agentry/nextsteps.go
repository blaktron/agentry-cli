// The "next steps" block the first-run commands end with. After a sign-in,
// a setup, an install into the runner home or a clean validate, the
// commands that follow are named, so a person is not left to find them in
// the docs. The block goes on stderr, beside the narration, and never under
// --json, whose stdout is the result and whose reader is a program.
package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/termtext"
)

// nextStep is one step of the block: a command line and, under it, what it
// does or how to fill it in, one line each.
type nextStep struct {
	command string
	notes   []string
}

// printNextSteps writes the block on w: a heading, then each step numbered
// with its notes indented under it. Nothing is written under --json.
func printNextSteps(w io.Writer, steps ...nextStep) {
	if flags.jsonOut || len(steps) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "next steps:")
	for i, s := range steps {
		_, _ = fmt.Fprintf(w, "  %d. %s\n", i+1, s.command)
		for _, n := range s.notes {
			_, _ = fmt.Fprintf(w, "     %s\n", n)
		}
	}
}

// loginNextSteps follows a sign-in: the machine still needs a model
// provider, and doctor says whether it is ready.
func loginNextSteps() []nextStep {
	return []nextStep{
		{command: "agentry setup", notes: []string{"choose the model provider QuickAgents run on, and prepare the container engine"}},
		{command: "agentry doctor", notes: []string{"check that this machine is ready to run one"}},
	}
}

// setupNextSteps follows a setup: check it, then find or run a QuickAgent.
func setupNextSteps() []nextStep {
	return []nextStep{
		{command: "agentry doctor", notes: []string{"check the key variable, the container engine and the registry"}},
		{command: "agentry search <words>", notes: []string{"find a QuickAgent to run"}},
		{command: "agentry run <ns/name>", notes: []string{"run one; it is installed on first use"}},
	}
}

// validateNextSteps follows a clean validate of dir: try it here, or
// submit it.
func validateNextSteps(dir string) []nextStep {
	return []nextStep{
		{command: "agentry run --local " + dir + " --allow-unsigned", notes: []string{"try it on this machine first"}},
		{command: "agentry publish " + dir, notes: []string{"submit it to the registry and follow the pipeline to its verdict"}},
	}
}

// installNextSteps follows an install into the runner home: the run line
// for ns/name, with the consent flag the release's attestation needs again
// at run time, --intake for each of its questions and --file where it
// declares the files it takes, then info for the full picture. A secret
// question is named but not put on the line. Each question's prompt is the
// publisher's text, cleaned for the terminal and cut to a line.
func installNextSteps(ns, name string, m *manifest.Manifest, consentFlag string) []nextStep {
	ref := ns + "/" + name
	line := "agentry run " + ref
	if consentFlag != "" {
		line += " " + consentFlag
	}
	var notes []string
	for _, q := range m.Intake {
		need := "optional"
		if q.Required {
			need = "required"
		}
		prompt := termtext.Safe(cutText(strings.TrimSpace(q.Prompt), maxInfoLine))
		if q.Type == manifest.IntakeSecret {
			// A secret is typed on the terminal when the run asks for it,
			// never put on a command line.
			notes = append(notes, q.ID+" ("+need+", a secret the run asks for on the terminal): "+prompt)
			continue
		}
		line += " --intake " + q.ID + "=<value>"
		note := q.ID + " (" + need + "): " + prompt
		if q.Type == manifest.IntakeChoice && len(q.Choices) > 0 {
			note += "; one of " + strings.Join(quoteEach(q.Choices), ", ")
		}
		notes = append(notes, note)
	}
	if m.Inputs != nil {
		line += " --file <path>"
		notes = append(notes, inputsLine(m)+" --file is repeatable.")
	}
	if len(m.Intake) == 0 && m.Inputs == nil {
		notes = append(notes, "the task comes from --input FILE or piped stdin, else it runs on its own instruction")
	}
	return []nextStep{
		{command: line, notes: notes},
		{command: "agentry info " + ref, notes: []string{"its questions in full, the files it takes, its keys and the publisher's guide"}},
	}
}

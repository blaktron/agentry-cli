// Package intake collects a BriefAgent's up-front questions (manifest v2
// `intake`, plan §3.5): asked before the sandbox starts, in either run
// mode, through the same prompter that collects MCP profile secrets, or
// answered non-interactively with --intake id=value. The answers are
// staged read-only for the agent at /inputs/intake.json; non-secret
// answers are receipts. A secret answer never reaches the agent or a
// receipt: it becomes the value of the declared capability secret of the
// same (upper-cased) name and is injected the way declared secrets are.
package intake

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/setup"
)

// Answers is what the user said, by question id.
type Answers struct {
	// Values are text and choice answers.
	Values map[string]string
	// Files are file answers: id → absolute host path (staged into /inputs).
	Files map[string]string
	// Secrets are secret answers: id → value. Never written anywhere but
	// the run's secrets file, under SecretName(id).
	Secrets map[string]string
	// FromSource are secret questions the run's secret source answers
	// (agentry-cli#402): their value never passes through here, since the
	// engine resolves the declared capability secret SecretName(id) from
	// that source itself; only the name is recorded.
	FromSource map[string]bool
}

// SecretName is the declared capability secret a secret intake answer
// becomes: the question id, upper-cased ("api_token" → API_TOKEN).
func SecretName(id string) string { return strings.ToUpper(id) }

// ParsePreset reads --intake id=value flags.
func ParsePreset(flags []string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range flags {
		id, val, ok := strings.Cut(f, "=")
		id = strings.TrimSpace(id)
		if !ok || id == "" {
			return nil, fmt.Errorf("--intake %q: want id=value", f)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("--intake %s given twice", id)
		}
		out[id] = val
	}
	return out, nil
}

// Collect answers every question: from preset first, then by asking p
// (nil when no terminal is available — a required question without a
// preset answer is then an error naming the flag). declaredSecrets are
// the manifest's capability secrets; a secret question whose upper-cased
// id is not among them has nowhere safe to go and is refused.
func Collect(p setup.Prompter, qs []manifest.IntakeQuestion, preset map[string]string, declaredSecrets []string) (*Answers, error) {
	return CollectFrom(p, qs, preset, declaredSecrets, nil)
}

// CollectFrom is Collect with the run's secret source in view. fromSource
// (nil: none) reports whether that source holds a declared secret. With no
// preset and no terminal, a secret question it holds is answered from the
// source rather than refused as unanswerable: a hosted run's envFile,
// filled from the account's vault, carries the key an intake question asks
// for (agentry-cli#402). A terminal still asks, so a person can mean
// another value than the source holds.
func CollectFrom(p setup.Prompter, qs []manifest.IntakeQuestion, preset map[string]string, declaredSecrets []string, fromSource func(name string) bool) (*Answers, error) {
	a := &Answers{Values: map[string]string{}, Files: map[string]string{}, Secrets: map[string]string{}, FromSource: map[string]bool{}}
	known := map[string]bool{}
	for _, q := range qs {
		known[q.ID] = true
	}
	for id := range preset {
		if !known[id] {
			return nil, fmt.Errorf("--intake %s: the BriefAgent asks no question with that id", id)
		}
	}
	declared := map[string]bool{}
	for _, s := range declaredSecrets {
		declared[s] = true
	}
	for _, q := range qs {
		typ := q.Type
		if typ == "" {
			typ = manifest.IntakeText
		}
		if typ == manifest.IntakeSecret && !declared[SecretName(q.ID)] {
			return nil, fmt.Errorf("intake question %q is a secret but the manifest declares no capability secret named %s to carry it", q.ID, SecretName(q.ID))
		}
		val, given := preset[q.ID]
		if !given && p == nil && typ == manifest.IntakeSecret && fromSource != nil && fromSource(SecretName(q.ID)) {
			a.FromSource[q.ID] = true
			continue
		}
		if !given && p != nil {
			var err error
			if val, err = ask(p, q, typ); err != nil {
				return nil, err
			}
		}
		if strings.TrimSpace(val) == "" {
			if q.Required {
				return nil, fmt.Errorf("intake question %q is required (answer it on a terminal or pass --intake %s=...)", q.ID, q.ID)
			}
			continue
		}
		if err := check(q, typ, val); err != nil {
			return nil, err
		}
		switch typ {
		case manifest.IntakeSecret:
			a.Secrets[q.ID] = val
		case manifest.IntakeFile:
			abs, err := filepath.Abs(val)
			if err != nil {
				return nil, fmt.Errorf("intake %s: %w", q.ID, err)
			}
			a.Files[q.ID] = abs
		default:
			a.Values[q.ID] = val
		}
	}
	return a, nil
}

func ask(p setup.Prompter, q manifest.IntakeQuestion, typ manifest.IntakeType) (string, error) {
	prompt := q.Prompt
	if q.Required {
		prompt += " (required)"
	}
	switch typ {
	case manifest.IntakeSecret:
		return p.AskSecret(prompt)
	case manifest.IntakeChoice:
		return p.Ask(prompt+" ["+strings.Join(q.Choices, "/")+"]", "")
	case manifest.IntakeFile:
		return p.Ask(prompt+" (path)", "")
	default:
		return p.Ask(prompt, "")
	}
}

func check(q manifest.IntakeQuestion, typ manifest.IntakeType, val string) error {
	switch typ {
	case manifest.IntakeChoice:
		for _, c := range q.Choices {
			if c == val {
				return nil
			}
		}
		return fmt.Errorf("intake %s: %q is not one of %s", q.ID, val, strings.Join(q.Choices, ", "))
	case manifest.IntakeFile:
		fi, err := os.Stat(val)
		if err != nil {
			return fmt.Errorf("intake %s: %w", q.ID, err)
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return fmt.Errorf("intake %s: %s is not a file or directory", q.ID, val)
		}
	}
	return nil
}

// Empty reports whether nothing was answered.
func (a *Answers) Empty() bool {
	return a == nil || (len(a.Values) == 0 && len(a.Files) == 0 && len(a.Secrets) == 0 && len(a.FromSource) == 0)
}

// Document is the agent-side /inputs/intake.json: values as given, files
// as their agent-side paths, secrets as the name of the secret only.
func (a *Answers) Document(inputsMount string) map[string]any {
	doc := map[string]any{}
	if a == nil {
		return doc
	}
	for id, v := range a.Values {
		doc[id] = v
	}
	for id, path := range a.Files {
		doc[id] = inputsMount + "/" + filepath.Base(path)
	}
	for id := range a.Secrets {
		doc[id] = map[string]string{"secret": SecretName(id)}
	}
	for id := range a.FromSource {
		doc[id] = map[string]string{"secret": SecretName(id)}
	}
	return doc
}

// Receipt is the receipts/intake.json content: the same as Document (it
// carries no secret values).
func (a *Answers) Receipt(inputsMount string) map[string]any { return a.Document(inputsMount) }

// FilePaths returns the host paths of file answers, sorted.
func (a *Answers) FilePaths() []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a.Files))
	for _, p := range a.Files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ErrNoTerminal explains why a required question could not be asked.
var ErrNoTerminal = errors.New("no terminal to ask intake questions on")

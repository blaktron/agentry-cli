// Package intake collects a QuickAgent's up-front questions (manifest v2
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
	"slices"
	"sort"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/setup"
	"github.com/blaktron/agentry-cli/pkg/runner/sources"
	"github.com/blaktron/agentry-cli/pkg/runner/targethost"
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
	// Sources are input-source answers (plan input-sources.md §5): id →
	// the source, fetched before the run into /inputs/<id>/. Its token,
	// when it has one, is filled in after Collect (sources.ResolveTokens)
	// and never written anywhere but the fetch step's stdin.
	Sources map[string]*sources.Source
	// Fetched is each source's fetch, set by the engine before it writes
	// intake.json, so the document names the commit and the files.
	Fetched map[string]sources.Fetched
	// Hosts are target-host answers (plan runtime-hosts.md §4;
	// agentry-cli#861): id → the host, its answer read here; its address,
	// key and sign-in are filled in after Collect (targethost.Prepare), and
	// its sign-in is never written anywhere but the host link's files.
	Hosts map[string]*targethost.Host
}

// SecretName is the declared capability secret a secret intake answer
// becomes: the question id, upper-cased ("api_token" → API_TOKEN).
func SecretName(id string) string { return strings.ToUpper(id) }

// SourceHosts is the host of each input source the answers name for qs:
// what a scheduled run will fetch, so the keys saved for those hosts can
// be sealed beside a schedule that runs while its person is signed out
// (agentry-cli#854). A type this CLI cannot fetch, or an answer it cannot
// read, is an error, as it is to a run.
func SourceHosts(qs []manifest.IntakeQuestion, answers map[string]string) ([]string, error) {
	var hosts []string
	for _, q := range qs {
		val := strings.TrimSpace(answers[q.ID])
		if !q.Type.IsSource() || val == "" {
			continue
		}
		kind, ok := sources.Lookup(q.Type)
		if !ok {
			return nil, sources.Unfetched(q.ID, q.Type)
		}
		src, err := kind.Parse(val)
		if err != nil {
			return nil, fmt.Errorf("intake %s: %w", q.ID, err)
		}
		if !slices.Contains(hosts, src.Host) {
			hosts = append(hosts, src.Host)
		}
	}
	return hosts, nil
}

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
	return CollectWithSource(p, qs, preset, declaredSecrets, nil)
}

// CollectWithSource is Collect with the run's secret source in view. fromSource
// (nil: none) reports whether that source holds a declared secret. With no
// preset and no terminal, a secret question it holds is answered from the
// source rather than refused as unanswerable: a hosted run's envFile,
// filled from the account's vault, carries the key an intake question asks
// for (agentry-cli#402). A terminal still asks, so a person can mean
// another value than the source holds.
func CollectWithSource(p setup.Prompter, qs []manifest.IntakeQuestion, preset map[string]string, declaredSecrets []string, fromSource func(name string) bool) (*Answers, error) {
	a := &Answers{Values: map[string]string{}, Files: map[string]string{}, Secrets: map[string]string{}, FromSource: map[string]bool{}, Sources: map[string]*sources.Source{}, Hosts: map[string]*targethost.Host{}}
	known := map[string]bool{}
	for _, q := range qs {
		known[q.ID] = true
	}
	for id := range preset {
		if !known[id] {
			return nil, fmt.Errorf("--intake %s: the QuickAgent asks no question with that id", id)
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
		// A source question is answered only when this CLI can fetch its
		// kind: an unknown type is never taken as text (agentry-cli#833).
		var kind sources.Kind
		if typ.IsSource() {
			k, ok := sources.Lookup(typ)
			if !ok {
				return nil, sources.Unfetched(q.ID, typ)
			}
			kind = k
		} else if !knownType(typ) {
			return nil, fmt.Errorf("intake question %q has the type %q, which this version of agentry does not know", q.ID, typ)
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
			if val, err = ask(p, q, typ, kind); err != nil {
				return nil, err
			}
		}
		if strings.TrimSpace(val) == "" {
			if q.Required && typ == manifest.IntakeSecret && fromSource != nil {
				return nil, fmt.Errorf("intake question %q is required (answer it on a terminal, pass --intake %s=..., or set the secret %s in the run's secret source)", q.ID, q.ID, SecretName(q.ID))
			}
			if q.Required {
				return nil, fmt.Errorf("intake question %q is required (answer it on a terminal or pass --intake %s=...)", q.ID, q.ID)
			}
			continue
		}
		if typ.IsSource() {
			src, err := kind.Parse(val)
			if err != nil {
				return nil, fmt.Errorf("intake %s: %w", q.ID, err)
			}
			src.ID = q.ID
			a.Sources[q.ID] = &src
			continue
		}
		if typ == manifest.IntakeHost {
			// A target host is never text: read here, readied before the
			// run (agentry-cli#861).
			ans, err := targethost.ParseAnswer(val)
			if err != nil {
				return nil, fmt.Errorf("intake %s: %w", q.ID, err)
			}
			a.Hosts[q.ID] = &targethost.Host{ID: q.ID, Answer: ans}
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

// knownType says whether this CLI answers an intake type that asks for no
// source.
func knownType(t manifest.IntakeType) bool {
	switch t {
	case manifest.IntakeText, manifest.IntakeSecret, manifest.IntakeChoice, manifest.IntakeFile, manifest.IntakeHost:
		return true
	}
	return false
}

func ask(p setup.Prompter, q manifest.IntakeQuestion, typ manifest.IntakeType, kind sources.Kind) (string, error) {
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
	case manifest.IntakeHost:
		return p.Ask(prompt+" ([user@]host[:port])", "")
	default:
		if typ.IsSource() {
			return p.Ask(prompt+" "+kind.Prompt, "")
		}
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
	return a == nil || (len(a.Values) == 0 && len(a.Files) == 0 && len(a.Secrets) == 0 && len(a.FromSource) == 0 && len(a.Sources) == 0 && len(a.Hosts) == 0)
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
	for id, s := range a.Sources {
		doc[id] = sources.Document(*s, a.Fetched[id].Source, inputsMount)
	}
	for id, h := range a.Hosts {
		doc[id] = hostDocument(h)
	}
	return doc
}

// hostDocument is a target host's intake.json entry: the host as named and
// as whom, and once readied the address and the key the run trusted;
// never its sign-in.
func hostDocument(h *targethost.Host) map[string]any {
	if h.Address.IsValid() {
		return h.Document()
	}
	return map[string]any{"host": map[string]any{"named": h.Answer.Host, "port": h.Answer.Port, "user": h.Answer.User}}
}

// SourceList answers the input sources by id, sorted.
func (a *Answers) SourceList() []*sources.Source {
	if a == nil {
		return nil
	}
	out := make([]*sources.Source, 0, len(a.Sources))
	for _, s := range a.Sources {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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

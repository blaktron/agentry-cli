package drafter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/knowledge"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Draft is draft.json (plan builder-rework.md §7.5, D15): what the Drafter
// returned, known fields only, and what the post-step adds (Finish, and the
// CLI's validation of the rendered package). A client reads it as data: its
// values reach the Builder only as text in the fields the Builder already
// has.
type Draft struct {
	// Manifest is the v2 manifest as JSON, as the Drafter wrote it.
	Manifest json.RawMessage `json:"manifest"`
	// Instructions is the instruction file (AGENT.md).
	Instructions string `json:"instructions"`
	// Notes is Markdown for the person.
	Notes         string   `json:"notes"`
	Matches       []Match  `json:"matches"`
	Sources       []Source `json:"sources"`
	OpenQuestions []string `json:"openQuestions"`

	// Drafter names what made the draft (the post-step's).
	Drafter *Made `json:"drafter,omitempty"`
	// AIReview is what AI Review read and withheld (the post-step's).
	AIReview *AIReview `json:"aiReview,omitempty"`
	// Knowledge is each of the person's knowledge packs the Drafter was
	// given, how it was scanned and what of it was withheld (the
	// post-step's; agentry-cli#522).
	Knowledge []PackRead `json:"knowledge,omitempty"`
	// Revises is the release a revision was drafted from (the post-step's;
	// agentry-cli#526): its ref and its archive's digest. Absent for a new
	// QuickAgent.
	Revises *Revises `json:"revises,omitempty"`
	// Validation is `agentry validate --json`'s report over the rendered
	// package (the post-step's): the manifest rules and the S1, S3 and S4
	// scanners. A draft that fails them is kept, with its problems, for the
	// person to fix (plan §7.5).
	Validation json.RawMessage `json:"validation,omitempty"`
	// ManifestError says why the draft could not be checked: its manifest
	// does not parse (the package is rendered all the same, for the person
	// to fix), its instruction path or its reports were refused (nothing is
	// rendered), or validation could not run. Validation is then absent.
	ManifestError string `json:"manifestError,omitempty"`
	// Recovered says the post-step took a schema-valid terminal object
	// from a last message the runner refused for text before it.
	Recovered bool `json:"recovered,omitempty"`

	// ownNotes is the notes as the Drafter wrote them, before the
	// post-step's sections; corrections is what Correct did; extras are
	// the post-step's other sections, in the order they were added.
	ownNotes      string
	corrections   []string
	extras        []string
	repaired      bool
	repairFailure string
	// lastMessage is an unparsed answer's last message (Unparsed).
	lastMessage string
	// serverProblems are the declared MCP servers whose own runtime range
	// excludes the run's, one rules line each; serversUnchecked the ones
	// whose metadata could not be read (checkServers).
	serverProblems   []string
	serversUnchecked []string
	// serverSigns are what to check before a run, and serversAdmitted what
	// admitted each server with no problem: a Runs here entry or the
	// Drafter's judgement (checkServers, agentry-cli#660).
	serverSigns     []string
	serversAdmitted []string
}

// Match is a published QuickAgent that does the task, or most of it.
type Match struct {
	Ref string `json:"ref"`
	Why string `json:"why"`
}

// Source is one page or registry answer the Drafter used, or one AI Review
// withheld.
type Source struct {
	URL      string `json:"url"`
	Used     string `json:"used"`
	Withheld bool   `json:"withheld,omitempty"`
}

// Made names the Drafter run behind a draft.
type Made struct {
	Ref   string `json:"ref"`
	Build string `json:"build,omitempty"`
	Model string `json:"model,omitempty"`
	RunID string `json:"runId,omitempty"`
	// Guide is the pack the Drafter ships and every run of it read
	// (GuideName; agentry-cli#657), recorded apart from the person's
	// packs in Knowledge. It is part of the Drafter's release, so it has
	// no scan record of its own.
	Guide *GuideRead `json:"guide,omitempty"`
}

// GuideRead is the record of the drafting guide a draft had: its name and
// the digest, pages and bytes its manifest.json gives.
type GuideRead struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Pages  int    `json:"pages"`
	Bytes  int64  `json:"bytes"`
}

// made is the record of what made a draft: this package at ref, the
// client's build, the run's model and id, and the guide it ships.
func made(env *receipt.Envelope, ref, build string) *Made {
	return &Made{Ref: ref, Build: build, Model: env.Harness.Model, RunID: env.RunID, Guide: guideRead()}
}

// guideRead reads the embedded guide's manifest.json; nil only if the
// binary carries none, which TestTheDraftingGuideLoadsFromTheEmbeddedTree
// rules out.
func guideRead() *GuideRead {
	raw, err := fs.ReadFile(files, manifest.KnowledgeDir+"/"+GuideName+"/"+knowledge.ManifestFile)
	if err != nil {
		return nil
	}
	var m knowledge.Manifest
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return &GuideRead{Name: m.Name, Digest: m.SHA256, Pages: m.Pages, Bytes: m.Bytes}
}

// AIReview is the draft's record of AI Review: the description's verdict,
// the run's receipt counts (what the web tools read, scanned and withheld,
// and where it was scanned), and each text the model was not given.
type AIReview struct {
	// Description is the verdict on the description and hints, read
	// before the Drafter started: always pass in a draft that exists.
	Description webscan.Verdict `json:"description"`
	receipt.Web
	// WithheldSources is every text AI Review kept from the model, with why.
	WithheldSources []WithheldSource `json:"withheldSources"`
	// Check is AI Review's check of the finished draft (the post-step's,
	// agentry-cli#418): absent for a draft that could not be rendered.
	Check *Check `json:"check,omitempty"`
}

// WithheldSource is one text the model was not given.
type WithheldSource struct {
	Tool   string `json:"tool"`
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

// Parse reads the Drafter's result, known fields only. The runner has held
// it to output.schema.json; this is the decode.
func Parse(result json.RawMessage) (*Draft, error) {
	var d Draft
	if err := json.Unmarshal(result, &d); err != nil {
		return nil, fmt.Errorf("the Drafter's answer is not a draft: %w", err)
	}
	var probe map[string]json.RawMessage
	if len(d.Manifest) == 0 || json.Unmarshal(d.Manifest, &probe) != nil {
		return nil, errors.New("the Drafter's answer has no manifest object")
	}
	// Only the Drafter's own fields are read from its answer; the
	// post-step's are Finish's and the CLI's to write.
	d.Drafter, d.AIReview, d.Knowledge, d.Validation, d.ManifestError = nil, nil, nil, nil, ""
	d.Recovered = false
	if d.Matches == nil {
		d.Matches = []Match{}
	}
	if d.Sources == nil {
		d.Sources = []Source{}
	}
	if d.OpenQuestions == nil {
		d.OpenQuestions = []string{}
	}
	return &d, nil
}

// The reports the Drafter writes its two long texts as, and the most read
// of each (the output schema's bounds on the same fields).
const (
	instructionsReport = "AGENT.md"
	notesReport        = "notes.md"
	maxInstructions    = 60000
	maxNotes           = 20000
)

// FromReports fills the instructions and the notes from the reports the
// Drafter wrote (plan §7.5): its last message carries the manifest and the
// lists, and each long text comes as a report of its own, which a model
// writes more reliably than one large JSON answer. A field the message
// carried is kept. A report that is not a regular file, or is over its
// bound, is not read, and the error says so.
func FromReports(d *Draft, reportsDir string) error {
	var errs []error
	for _, r := range []struct {
		name  string
		max   int
		field *string
	}{{instructionsReport, maxInstructions, &d.Instructions}, {notesReport, maxNotes, &d.Notes}} {
		if strings.TrimSpace(*r.field) != "" {
			continue
		}
		text, err := readReport(reportsDir, r.name, r.max)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if text != "" {
			*r.field = text
		}
	}
	return errors.Join(errs...)
}

// readReport reads one report the Drafter wrote: "" when it wrote none, and
// an error for one that is not a regular text file of at most maxBytes.
func readReport(reportsDir, name string, maxBytes int) (string, error) {
	p := filepath.Join(reportsDir, name)
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > int64(maxBytes) {
		return "", fmt.Errorf("the report %s is not a text of at most %d bytes", name, maxBytes)
	}
	b, err := os.ReadFile(p) //nolint:gosec // the run's own reports
	if err != nil || !utf8.Valid(b) {
		return "", fmt.Errorf("the report %s is not text", name)
	}
	return string(b), nil
}

// instructionRE is an instruction file the rendered package may hold: one
// file name at the package's root.
var instructionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.md$`)

// Render writes the draft as a package into dir, which must not exist yet:
// agent.yaml, rendered from the manifest in the Drafter's own key order; the
// instruction file the manifest names (AGENT.md when it names none); and,
// as `agentry init` does, a README.md from the manifest's own summary and
// overview and a LICENSE placeholder naming its SPDX id, since public
// publication needs both (V7) and the person writes the real ones.
//
// A manifest pkg/manifest cannot parse is still rendered, so the person
// fixes it in the package, and the parse error is returned with it. An
// instruction path that is not one Markdown file name at the root is
// refused, and nothing is written: a draft is agent-written, and its paths
// are data.
func Render(d *Draft, dir string) (*manifest.Manifest, error) {
	yml, err := manifestYAML(d.Manifest)
	if err != nil {
		return nil, err
	}
	var loose map[string]any
	_ = json.Unmarshal(d.Manifest, &loose)
	m, perr := manifest.Parse(yml)
	if perr != nil {
		perr = fmt.Errorf("the draft's manifest does not parse: %w", perr)
		m = nil
	}
	inst := looseString(loose, "entrypoints", "instruction")
	if m != nil {
		inst = m.Entrypoints.Instruction
	}
	if inst == "" {
		inst = manifest.DefaultInstructionFile
	}
	if !instructionRE.MatchString(inst) {
		return m, fmt.Errorf("the draft names its instruction file %q; a draft's instruction is one Markdown file at the package's root", inst)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return m, fmt.Errorf("make the draft's package folder: %w", err)
	}
	// The instruction file is the draft's own; a README or a LICENSE of the
	// same name is not written over it.
	out := map[string][]byte{
		"README.md": []byte(readme(looseString(loose, "name"), looseString(loose, "summary"), looseString(loose, "guide", "overview"))),
		"LICENSE":   []byte(licenseNote(looseString(loose, "license"))),
	}
	out["agent.yaml"] = yml
	out[inst] = []byte(d.Instructions)
	for name, body := range out {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil { //nolint:gosec // the person's own output folder
			return m, fmt.Errorf("write the draft's %s: %w", name, err)
		}
	}
	return m, perr
}

// NoteWithheld adds to the notes the texts AI Review kept from the model,
// with why (plan §7.3: the notes list every withheld source), from the
// draft's AI Review record rather than from what the model chose to say.
func (d *Draft) NoteWithheld() {
	if d.AIReview == nil || len(d.AIReview.WithheldSources) == 0 {
		return
	}
	var b strings.Builder
	head := "The Drafter was given the address and the reason for each of these, never the text:"
	for _, w := range d.AIReview.WithheldSources {
		if w.Tool == "knowledge" {
			head = "The Drafter was given the address and the reason for each of these, never the text; a file of a knowledge pack was left out of the pack it read:"
			break
		}
	}
	b.WriteString("## Withheld by AI Review\n\n" + head + "\n\n")
	for _, w := range d.AIReview.WithheldSources {
		fmt.Fprintf(&b, "- %s (%s): %s\n", w.URL, w.Tool, w.Reason)
	}
	d.noteSection(b.String())
}

// looseString reads a string at a path of the manifest as it came, or "":
// what renders the README and the LICENSE note of a manifest whose other
// fields may not parse.
func looseString(v map[string]any, path ...string) string {
	var cur any = v
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[k]
	}
	s, _ := cur.(string)
	return s
}

// readme is the rendered package's README: what the draft says of itself,
// marked as a draft.
func readme(name, summary, overview string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n%s\n", name, summary)
	if overview != "" {
		fmt.Fprintf(&b, "\n%s\n", overview)
	}
	b.WriteString("\nDrafted by the Drafter (`agentry draft`). Review the manifest, the instructions and this file before you publish;\n" +
		"this README renders on the QuickAgent's page.\n")
	return b.String()
}

// licenseNote is the LICENSE placeholder, as `agentry init` writes one.
func licenseNote(spdx string) string {
	if spdx == "" {
		spdx = "(none yet)"
	}
	return "PLACEHOLDER LICENSE NOTE\n\nagent.yaml declares the SPDX id \"" + spdx + "\". Replace this file with the\n" +
		"full text of the license you actually chose before publishing publicly\n" +
		"(rule V7 requires a LICENSE file consistent with the declared SPDX id).\n"
}

// manifestYAML renders a manifest given as JSON as block YAML, keeping its
// keys in the order they came; a string that would read back as another
// type stays quoted, and one of several lines is a literal block.
func manifestYAML(raw json.RawMessage) ([]byte, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("the draft's manifest is not JSON: %w", err)
	}
	if len(n.Content) != 1 || n.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the draft's manifest is not an object")
	}
	var block func(*yaml.Node)
	block = func(x *yaml.Node) {
		switch x.Kind {
		case yaml.MappingNode, yaml.SequenceNode:
			x.Style = 0
		case yaml.ScalarNode:
			if x.Tag == "!!str" {
				x.Style = 0
			}
		}
		for _, c := range x.Content {
			block(c)
		}
	}
	block(&n)
	var b bytes.Buffer
	b.WriteString("# drafted by the Drafter (agentry draft); review every line before you submit it\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

package drafter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"
)

// Draft is draft.json (plan builder-rework.md §7.5, D15): what the Drafter
// returned, known fields only, and what the CLI's post-step adds. A client
// reads it as data: its values reach the Builder only as text in the fields
// the Builder already has.
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
	// Validation is `agentry validate --json`'s report over the rendered
	// package (the post-step's): the manifest rules and the S1, S3 and S4
	// scanners. A draft that fails them is kept, with its problems, for the
	// person to fix (plan §7.5).
	Validation json.RawMessage `json:"validation,omitempty"`
	// ManifestError says why the manifest could not be read at all; the
	// package is then not rendered.
	ManifestError string `json:"manifestError,omitempty"`
}

// Match is a published BriefAgent that does the task, or most of it.
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
}

// AIReview is the draft's record of AI Review: the description's scan, and
// what the web tools read and withheld, from the run's receipt and ledger.
type AIReview struct {
	// Scan is where the texts were scanned: "jev" (the person's own key)
	// or "registry" (Agentry's, with the job's credential).
	Scan string `json:"scan"`
	// Description is the verdict on the description and hints, read
	// before the Drafter started: always "pass" in a draft that exists.
	Description string `json:"description"`
	Read        int    `json:"read"`
	Passed      int    `json:"passed"`
	Withheld    int    `json:"withheld"`
	NotScanned  int    `json:"notScanned"`
	Model       string `json:"model,omitempty"`
	QuestionSet string `json:"questionSet,omitempty"`
	// WithheldSources is every text AI Review kept from the model, with why.
	WithheldSources []WithheldSource `json:"withheldSources"`
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
	// post-step's are the CLI's to write.
	d.Drafter, d.AIReview, d.Validation, d.ManifestError = nil, nil, nil, ""
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

// instructionRE is an instruction file the rendered package may hold: one
// file name at the package's root.
var instructionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.md$`)

// Render writes the draft as a package into dir, which must not exist yet:
// agent.yaml, rendered from the manifest in the Drafter's own key order; the
// instruction file the manifest names (AGENT.md when it names none); and,
// as `agentry init` does, a README.md from the manifest's own summary and
// overview and a LICENSE placeholder naming its SPDX id, since public
// publication needs both (V7) and the person writes the real ones.
// The manifest is parsed with pkg/manifest first; one that does not parse
// is not rendered, and the error says why. An instruction path that is not
// one Markdown file name at the root is refused rather than written: a
// draft is agent-written, and its paths are data.
func Render(d *Draft, dir string) (*manifest.Manifest, error) {
	yml, err := ManifestYAML(d.Manifest)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Parse(yml)
	if err != nil {
		return nil, fmt.Errorf("the draft's manifest does not parse: %w", err)
	}
	inst := m.Entrypoints.Instruction
	if inst == "" {
		inst = manifest.DefaultInstructionFile
	}
	if !instructionRE.MatchString(inst) {
		return m, fmt.Errorf("the draft names its instruction file %q; a draft's instruction is one Markdown file at the package's root", inst)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return m, err
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), yml, 0o644); err != nil { //nolint:gosec // the person's own output folder
		return m, err
	}
	if err := os.WriteFile(filepath.Join(dir, inst), []byte(d.Instructions), 0o644); err != nil { //nolint:gosec // ditto
		return m, err
	}
	for name, body := range map[string]string{"README.md": readme(m), "LICENSE": licenseNote(m.License)} {
		if name == inst {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil { //nolint:gosec // ditto
			return m, err
		}
	}
	return m, nil
}

// readme is the rendered package's README: what the draft says of itself,
// marked as a draft.
func readme(m *manifest.Manifest) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# %s\n\n%s\n", m.Name, m.Summary)
	if m.Guide != nil && m.Guide.Overview != "" {
		fmt.Fprintf(&b, "\n%s\n", m.Guide.Overview)
	}
	b.WriteString("\nDrafted by the Drafter (`agentry draft`). Review the manifest, the instructions and this file before you publish;\n" +
		"this README renders on the BriefAgent's page.\n")
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

// ManifestYAML renders a manifest given as JSON as block YAML, keeping its
// keys in the order they came; a string that would read back as another
// type stays quoted, and one of several lines is a literal block.
func ManifestYAML(raw json.RawMessage) ([]byte, error) {
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

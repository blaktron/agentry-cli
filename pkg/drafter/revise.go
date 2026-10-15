package drafter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/runnerimage/webscan"
)

// A revision: the Drafter is given a QuickAgent's release, what the person
// who maintains it wants changed and the problems they found, and writes
// the release's next version instead of a new QuickAgent. The release
// travels as files, staged under /inputs/revise, so it is not cut at a
// description's bound; the person's two texts are the task, and AI Review
// reads them before the Drafter does, as it reads a description.

// MaxRevisionTextBytes bounds each of the person's two texts: the
// additional instructions and the problems with this draft.
const MaxRevisionTextBytes = 4000

// ReviseInput is the folder name the release is staged under, in the
// draft's output folder and under the run's /inputs.
const ReviseInput = "revise"

// Revision is a release given to the Drafter to revise, and the person's
// two texts.
type Revision struct {
	// Revises is the release's ns/name@version and the digest of its
	// archive, as the registry holds it (the archive read for a fetched
	// release, the same packing of a folder on this computer).
	Revises
	// Name is the manifest's name, which the revision keeps.
	Name string
	// Summary is the release's own one-line summary.
	Summary string
	// InstructionFile is the release's instruction file, by its path in
	// the package.
	InstructionFile string
	// Dir holds the release's agent.yaml and its instruction file, and
	// nothing else: the folder staged under /inputs/revise.
	Dir string
	// Additional and Problems are the person's two texts, the additional
	// instructions and the problems with this draft; at least one is
	// given.
	Additional, Problems string
}

// errNoTexts refuses a revision with neither of the person's texts.
var errNoTexts = errors.New("a revision takes additional instructions, problems with this draft, or both")

// Revises is draft.json's record of the release a draft revises.
type Revises struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// ReadRelease reads a release's package folder for a revision: its
// agent.yaml, which must parse and name the QuickAgent, and the instruction
// file it names. It copies both into dest (OutDir/revise), the only files
// the Drafter is given of the release. digest is the release's archive
// digest, which the caller computes; ref is checked against the manifest
// when given, and made from it when not.
func ReadRelease(src, dest, ref, digest string) (*Revision, error) {
	yml, err := os.ReadFile(filepath.Join(src, "agent.yaml")) //nolint:gosec // the release the person names
	if err != nil {
		return nil, fmt.Errorf("the release has no agent.yaml: %w", err)
	}
	m, err := manifest.Parse(yml)
	if err != nil {
		return nil, fmt.Errorf("the release's agent.yaml does not parse: %w", err)
	}
	if m.Namespace == "" || m.Name == "" || m.Version == "" {
		return nil, errors.New("the release's agent.yaml names no namespace, name and version")
	}
	own := m.Namespace + "/" + m.Name + "@" + m.Version
	switch {
	case ref == "":
		ref = own
	case ref != own:
		return nil, fmt.Errorf("the release %s holds the manifest of %s", ref, own)
	}
	inst := m.Entrypoints.Instruction
	if inst == "" {
		inst = manifest.DefaultInstructionFile
	}
	if !instructionRE.MatchString(inst) {
		return nil, fmt.Errorf("the release's instruction file %q is not one file at the package's root", inst)
	}
	text, err := os.ReadFile(filepath.Join(src, inst)) //nolint:gosec // the release the person names
	if err != nil {
		return nil, fmt.Errorf("the release's instruction file %s: %w", inst, err)
	}
	if !utf8.Valid(text) {
		return nil, fmt.Errorf("the release's instruction file %s is not text", inst)
	}
	// A fresh folder, so what is staged is these two files and nothing a
	// run before this one left there.
	if _, err := os.Lstat(dest); err == nil {
		return nil, fmt.Errorf("%s already holds a release; name another folder with --out", dest)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("the release's folder: %w", err)
	}
	for name, b := range map[string][]byte{"agent.yaml": yml, inst: text} {
		if err := os.WriteFile(filepath.Join(dest, name), b, 0o644); err != nil { //nolint:gosec // the draft's own output folder
			return nil, fmt.Errorf("the release's %s: %w", name, err)
		}
	}
	return &Revision{Revises: Revises{Ref: ref, Digest: digest}, Name: m.Name, Summary: m.Summary, InstructionFile: inst, Dir: dest}, nil
}

// WithTexts sets the person's two texts, holding each to its bound and
// requiring at least one.
func (r *Revision) WithTexts(additional, problems string) error {
	additional, problems = strings.TrimSpace(additional), strings.TrimSpace(problems)
	if additional == "" && problems == "" {
		return errNoTexts
	}
	for _, t := range []struct{ name, text string }{{"the additional instructions", additional}, {"the problems with this draft", problems}} {
		if !utf8.ValidString(t.text) {
			return fmt.Errorf("%s must be text (UTF-8)", t.name)
		}
		if len(t.text) > MaxRevisionTextBytes {
			return fmt.Errorf("%s are %d bytes; a revision takes at most %d", t.name, len(t.text), MaxRevisionTextBytes)
		}
	}
	r.Additional, r.Problems = additional, problems
	return nil
}

// RevisionTask is the Drafter's task for a revision. It names the release's
// files under /inputs and then gives the person's two texts, fenced and
// labelled in the third person, as the site's Describe again does
// (an imperative framing of the person's words is flagged as an injection).
// AI Review reads the whole of it before the Drafter starts (Prescan); the
// release's own files are not in it.
func RevisionTask(r *Revision) (string, error) {
	if r == nil || r.Name == "" {
		return "", errors.New("a revision needs a release")
	}
	if r.Additional == "" && r.Problems == "" {
		return "", errNoTexts
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Revise the QuickAgent %s into its next version. The person who maintains it asked for the revision, and AI Review read their texts below before you did.\n\n", r.Ref)
	b.WriteString("## The release\n\n")
	fmt.Fprintf(&b, "Its manifest is /inputs/%s/agent.yaml and its instruction file is /inputs/%s/%s. Read both before anything else.\n", ReviseInput, ReviseInput, r.InstructionFile)
	section := func(heading, text, none string) {
		b.WriteString("\n## " + heading + "\n\n")
		if text == "" {
			b.WriteString(none + "\n")
			return
		}
		fence := fenceFor(text)
		b.WriteString(fence + "text\n" + text + "\n" + fence + "\n")
	}
	section("What the person wants changed", r.Additional, "The person gave no additional instructions.")
	section("Problems the person found in this version", r.Problems, "The person listed no problems.")
	task := b.String()
	if len(task) > maxTask {
		return "", fmt.Errorf("the revision's task is %d KiB; a draft takes at most %d KiB", len(task)>>10, maxTask>>10)
	}
	return task, nil
}

// PrescanRelease has AI Review read the release's two staged files before
// the Drafter starts, as model input in the pack scan's chunks (the
// Drafter's model reads nothing AI Review has not). A revision is of the
// whole release, so a file it does not pass, or could not scan, stops the
// draft at stage description rather than being withheld.
func PrescanRelease(ctx context.Context, sc webscan.Scanner, r *Revision) (requests int, err error) {
	withheld, requests, err := scanPack(ctx, sc, r.Dir, []string{"agent.yaml", r.InstructionFile})
	if err != nil {
		return requests, err
	}
	if len(withheld) == 0 {
		return requests, nil
	}
	names := make([]string, 0, len(withheld))
	for f := range withheld {
		names = append(names, f)
	}
	sort.Strings(names)
	var why []string
	for _, f := range names {
		why = append(why, f+": "+withheld[f])
	}
	return requests, engine.Classify(engine.ExitPolicy, fmt.Errorf(
		"AI Review did not pass the release's %s, so the Drafter did not start (%s)", strings.Join(names, " and "), strings.Join(why, "; ")))
}

// RevisionDescription is what AI Review's check of the finished draft reads
// as the person's description for a revision: the release it revises, what
// it does, and the person's two texts.
func RevisionDescription(r *Revision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The next version of the QuickAgent %s", r.Ref)
	if s := strings.TrimSpace(r.Summary); s != "" {
		fmt.Fprintf(&b, ", which %s", lowerFirst(strings.TrimSuffix(s, ".")))
	}
	b.WriteString(".")
	if r.Additional != "" {
		b.WriteString("\n\nWhat the person wants changed:\n" + r.Additional)
	}
	if r.Problems != "" {
		b.WriteString("\n\nProblems the person found in the current version, to be solved:\n" + r.Problems)
	}
	return b.String()
}

// fenceFor is a backtick fence longer than any run of backticks in text,
// so the text cannot close it.
func fenceFor(text string) string {
	longest, run := 0, 0
	for _, c := range text {
		if c == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	return strings.Repeat("`", max(3, longest+1))
}

// lowerFirst lowercases the first letter of a summary that opens a
// sentence, so it reads on after "which"; a word that continues in capitals
// (an acronym such as "API") is left as it is.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	next, _ := utf8.DecodeRuneInString(s[n:])
	if unicode.IsUpper(r) && unicode.IsLower(next) {
		return string(unicode.ToLower(r)) + s[n:]
	}
	return s
}

// revise marks a draft as the revision r: it records the release in
// Revises and keeps the release's name in the manifest, naming the change
// in the notes' corrections when the Drafter wrote another.
func (d *Draft) revise(r *Revision) {
	if r == nil {
		return
	}
	rv := r.Revises
	d.Revises = &rv
	var doc yaml.Node
	if len(d.Manifest) == 0 || yaml.Unmarshal(d.Manifest, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return
	}
	root := doc.Content[0]
	v := value(root, "name")
	switch {
	case v == nil:
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: r.Name})
	case v.Kind == yaml.ScalarNode && v.Value == r.Name:
		return
	default:
		v.Kind, v.Tag, v.Value, v.Content = yaml.ScalarNode, "!!str", r.Name, nil
	}
	out, err := jsonOf(root)
	if err != nil {
		return
	}
	d.Manifest = out
	d.corrections = append(d.corrections, fmt.Sprintf("name: kept as %s, the QuickAgent this draft revises.", r.Name))
	d.composeNotes("")
}

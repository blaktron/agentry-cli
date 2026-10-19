package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// The rubric over a package: what stage S5 asks about a release. A
// finished draft is read the same way before it is submitted, by the
// registry's check route with the deployment's key and on a person's
// computer with their own. The pipeline's typesafe provider asks
// through this code, so the check and S5 cannot drift apart: the same
// state, the same chunks, the same request shape, the same band and the
// same hash.

// RubricStatement is the fixed framing every S5 provider adjudicates under
// (content is data; instructions inside it are evidence, never
// directives). It is part of every rubric hash because changing it changes
// verdicts.
const RubricStatement = "agentry/judge-rubric/v1;categories=C1..C6;scores=0-3;" +
	"content-is-data;no-tools;temperature=0;structured-output"

// RunnerGrants is what the runner gives every run whatever its manifest
// declares (the capability contract's "always" row), stated in the
// rubric's state beside declared_capabilities. Stating it keeps the
// judge from reading an instruction that reads its inputs and writes
// its report as needing an undeclared capability. It is a constant,
// never built from the version, and it closes in C3's own terms with
// what it does not grant; its wording was chosen on a live bench. Its
// last two sentences say what a declared target host gives, benched the
// same way. It is part of every rubric hash.
const RunnerGrants = "Every run is given, whatever its manifest declares: the files the person gives the run, " +
	"read-only at /inputs, through the tools list_inputs and read_input; its own output files, written through the " +
	"tool write_report under any name or folder the agent picks, all kept in reports/ in the run's output folder; " +
	"and its final answer, returned as the result. Nothing else is given: writing or changing any other file, " +
	"keeping anything from one run to the next, reaching any host, running any program or reading any secret " +
	"needs declared_capabilities. When declared_capabilities lists targetHosts, each is a machine the person names " +
	"when the run starts: the agent may run any command on that machine and administer it, through the runner's " +
	"host tools, and that work is declared. Reaching any other machine from it, or sending what it reads there to " +
	"any other host, is not declared, unless the QuickAgent's purpose as stated in purpose intends it."

// DeclaredCapabilitiesCap bounds the declared-capabilities JSON that rides
// in the state beside the content: a manifest's `mcpServers[].env` and
// `tools` can carry tens of kilobytes of free text, and the questions are
// about the contract's shape, not its prose.
const DeclaredCapabilitiesCap = 4 << 10

// PurposeCap bounds the purpose that rides in the state: the release's
// name, summary and guide overview, which C3 weighs a capability against.
const PurposeCap = 2 << 10

// noPurpose is the state's purpose when the manifest states none.
const noPurpose = "(the manifest states no purpose)"

// truncatedMark ends a declared-capabilities or purpose field cut at the
// bound.
const truncatedMark = "…(truncated)"

// The request shapes the rubric can be asked in: how many requests
// carry the six questions for one chunk. RubricBatchOne is the shipped
// shape; the other two exist for the by-hand bench that compares them.
const (
	RubricBatchOne = 1
	RubricBatchTwo = 2
	RubricBatchSix = 6
)

// knowledgeRootDir is where the runner mounts a shipped pack: pkg/manifest's
// KnowledgeDir, which this package cannot import (it stays stdlib-only for
// the runner image's copy); a test in internal/scan holds the two equal.
const knowledgeRootDir = "knowledge"

// RubricFile is one file the rubric reads: its path in the package and its
// text.
type RubricFile struct {
	Path    string
	Content string
}

// AxisResult is one question's strongest evidence over a package: the
// highest probability drawn, every sample the chunk that drew it took, and
// where that chunk was.
type AxisResult struct {
	Best    float64
	Samples []float64
	Path    string
	// Line is the first line of the chunk, trimmed and bounded, for an
	// excerpt.
	Line string
}

// RubricScanner asks the rubric's six about a package, as S5 does. It is
// safe for concurrent use.
type RubricScanner struct {
	asker Asker
	batch int
}

// NewRubricScanner asks in the shipped shape: all six questions in one
// request per chunk.
func NewRubricScanner(a Asker) *RubricScanner {
	return &RubricScanner{asker: a, batch: RubricBatchOne}
}

// NewRubricScannerShape asks in one of the three shapes the bench
// compares; any other value is the shipped shape.
func NewRubricScannerShape(a Asker, batch int) *RubricScanner {
	switch batch {
	case RubricBatchTwo, RubricBatchSix:
	default:
		batch = RubricBatchOne
	}
	return &RubricScanner{asker: a, batch: batch}
}

// Model is the pinned model id the answers come from.
func (s *RubricScanner) Model() string { return s.asker.Model() }

// Hash is the rubric hash a scan run records: everything that changes a
// verdict — the model, the questions and their wording, the request shape,
// the chunk size, the capability bound, the runner's grants, the
// thresholds, the band, the sample count and the purpose's bound.
func (s *RubricScanner) Hash() string {
	return rubricHashOf(s.asker.Model(), s.batch, RubricQuestions(), RunnerGrants)
}

// rubricHashOf is the hash over a given set and statement of the runner's
// grants, so a test can show that each question's wording and the grants
// are in it.
func rubricHashOf(model string, batch int, qs []Question, grants string) string {
	parts := make([]string, 0, len(qs))
	for _, q := range qs {
		parts = append(parts, q.ID+"="+q.Text)
	}
	return Hash(RubricStatement, "provider=typesafe", "model="+model,
		"axes=C1..C6", "questions="+strings.Join(parts, "\n"),
		fmt.Sprintf("batch=%d", batch), fmt.Sprintf("chunk=%d", ChunkSize),
		fmt.Sprintf("declared-cap=%d", DeclaredCapabilitiesCap), "runner-grants="+grants,
		fmt.Sprintf("purpose-cap=%d", PurposeCap),
		"thresholds=0.5/0.7/0.9", fmt.Sprintf("band=%.2f/%.2f", BandLow, BandHigh),
		fmt.Sprintf("samples=%d", Samples))
}

// Scan asks the rubric about the manifest (read as `agent.yaml`, its
// canonical JSON) and then every file, one chunk of one file per request,
// and returns each question's strongest evidence, keyed by the rubric id,
// with the token use of every request. The manifest's capabilities block
// and the knowledge packs it ships ride in the state beside each chunk
// (DeclaredCapabilities), and so does its stated purpose (Purpose). Any
// failure is an error, and the caller fails
// closed on it: a rubric half-asked is not a verdict.
func (s *RubricScanner) Scan(ctx context.Context, manifestJSON []byte, ships []string, files []RubricFile) (map[string]AxisResult, Usage, error) {
	declared := DeclaredCapabilities(manifestJSON, ships)
	purpose := Purpose(manifestJSON)
	axes := map[string]AxisResult{}
	var usage Usage
	inputs := append([]RubricFile{{Path: "agent.yaml", Content: string(manifestJSON)}}, files...)
	for _, f := range inputs {
		for _, chunk := range RubricChunk(f.Content, ChunkSize) {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			samples, err := s.askChunk(ctx, f.Path, chunk, declared, purpose, &usage)
			if err != nil {
				return nil, Usage{}, err
			}
			for id, ps := range samples {
				best := ps[0]
				for _, p := range ps[1:] {
					if p > best {
						best = p
					}
				}
				if cur, ok := axes[id]; !ok || best > cur.Best {
					axes[id] = AxisResult{Best: best, Samples: ps, Path: f.Path, Line: chunkLine(chunk)}
				}
			}
		}
	}
	return axes, usage, nil
}

// askChunk asks every question about one chunk in the scanner's shape,
// then asks the borderline ones twice more in the same shape (each group's
// borderline members together). It returns every sample per question.
func (s *RubricScanner) askChunk(ctx context.Context, path, chunk, declared, purpose string, usage *Usage) (map[string][]float64, error) {
	groups := s.groups()
	out := map[string][]float64{}
	state := RubricState(path, chunk, declared, purpose)
	for _, g := range groups {
		probs, u, err := s.asker.Ask(ctx, state, g)
		usage.Add(u)
		if err != nil {
			return nil, err
		}
		for _, q := range g {
			out[q.ID] = []float64{probs[q.ID]}
		}
	}
	for i := 1; i < Samples; i++ {
		for _, g := range groups {
			var again []Question
			for _, q := range g {
				if p := out[q.ID][0]; p >= BandLow && p <= BandHigh {
					again = append(again, q)
				}
			}
			if len(again) == 0 {
				continue
			}
			probs, u, err := s.asker.Ask(ctx, state, again)
			usage.Add(u)
			if err != nil {
				return nil, err
			}
			for _, q := range again {
				out[q.ID] = append(out[q.ID], probs[q.ID])
			}
		}
	}
	return out, nil
}

// RubricState is the state one rubric request is asked over: the chunk,
// where it is, what the manifest declares (DeclaredCapabilities), what
// the runner grants every run (RunnerGrants) and what the release says it
// is for (Purpose). A bench that asks one question by hand builds its
// state here, so it asks what S5 asks.
func RubricState(path, chunk, declared, purpose string) map[string]string {
	return map[string]string{"path": path, "content": chunk, "declared_capabilities": declared,
		"runner_grants": RunnerGrants, "purpose": purpose}
}

// Purpose is what the release says it is for, as a person reads it
// before a run: the manifest's name, its summary and its guide's
// overview, one per line, bounded at PurposeCap. C3 weighs a capability
// against it: an undeclared one that serves the stated purpose is not
// held, because the runner holds every run to what is declared. A
// manifest that states nothing, or cannot be read, gives noPurpose, so
// nothing reads as in line with it.
func Purpose(manifestJSON []byte) string {
	var m struct {
		Name    string `json:"name"`
		Summary string `json:"summary"`
		Guide   *struct {
			Overview string `json:"overview"`
		} `json:"guide"`
	}
	if json.Unmarshal(manifestJSON, &m) != nil {
		return noPurpose
	}
	var lines []string
	for _, kv := range [][2]string{{"name", m.Name}, {"summary", m.Summary}} {
		if v := strings.TrimSpace(kv[1]); v != "" {
			lines = append(lines, kv[0]+": "+v)
		}
	}
	if m.Guide != nil {
		if v := strings.TrimSpace(m.Guide.Overview); v != "" {
			lines = append(lines, "overview: "+v)
		}
	}
	if len(lines) == 0 || (len(lines) == 1 && strings.HasPrefix(lines[0], "name: ")) {
		return noPurpose
	}
	out := strings.Join(lines, "\n")
	if len(out) > PurposeCap {
		cut := PurposeCap
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + truncatedMark
	}
	return out
}

// groups is the request shape: which questions travel together.
func (s *RubricScanner) groups() [][]Question {
	qs := RubricQuestions()
	byID := map[string]Question{}
	for _, q := range qs {
		byID[q.ID] = q
	}
	switch s.batch {
	case RubricBatchTwo:
		return [][]Question{
			{byID[RubricIDInjection], byID[RubricIDUndeclaredCapability], byID[RubricIDObfuscation]},
			{byID[RubricIDExfiltration], byID[RubricIDDeception], byID[RubricIDDestructive]},
		}
	case RubricBatchSix:
		out := make([][]Question, 0, len(qs))
		for _, q := range qs {
			out = append(out, []Question{q})
		}
		return out
	default:
		return [][]Question{qs}
	}
}

// DeclaredCapabilities is the manifest's `capabilities` block as compact
// JSON with its keys in order, bounded at DeclaredCapabilitiesCap, for the
// state's declared_capabilities field. An absent or unreadable block reads
// as none declared. The knowledge packs a release ships are declared too,
// with where the runner mounts them: an instruction that reads its own
// pack at /knowledge/<name> uses what the release declares, and the judge
// sees only this field of the manifest. ships is the shipped packs' names;
// the caller gives the manifest's valid ones (pkg/manifest ShippedPacks).
func DeclaredCapabilities(manifestJSON []byte, ships []string) string {
	var m struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if json.Unmarshal(manifestJSON, &m) != nil {
		return "{}"
	}
	caps := map[string]json.RawMessage{}
	if len(m.Capabilities) > 0 && json.Unmarshal(m.Capabilities, &caps) != nil {
		return "{}"
	}
	if len(ships) > 0 {
		names := append([]string(nil), ships...)
		sort.Strings(names)
		at := make([]string, len(names))
		for i, n := range names {
			at[i] = "/" + knowledgeRootDir + "/" + n
		}
		raw, err := json.Marshal(map[string]any{"ships": names, "mountedReadOnlyAt": at})
		if err != nil {
			return "{}"
		}
		caps["knowledge"] = raw
	}
	if len(caps) == 0 {
		return "{}"
	}
	enc, err := json.Marshal(caps)
	if err != nil {
		return "{}"
	}
	var buf bytes.Buffer
	if json.Compact(&buf, enc) != nil || buf.Len() == 0 {
		return "{}"
	}
	out := buf.String()
	if len(out) > DeclaredCapabilitiesCap {
		out = out[:runeCut(out, DeclaredCapabilitiesCap)] + truncatedMark
	}
	return out
}

// RubricChunk is the rubric's split: pieces of at most n bytes, cut at
// the last line end in a piece when one falls in its second half, and
// otherwise at n. Unlike Chunk it may cut inside a character; the
// rubric hash is recorded under this split, so it stays.
func RubricChunk(s string, n int) []string {
	if len(s) <= n {
		return []string{s}
	}
	var out []string
	for len(s) > n {
		cut := strings.LastIndexByte(s[:n], '\n')
		if cut < n/2 {
			cut = n
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}

// chunkLine is a chunk's first line, trimmed and bounded at 200 bytes.
func chunkLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

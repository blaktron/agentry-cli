package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The rubric over a package: what stage S5 asks about a release, moved
// here from the pipeline (agentry#708, plan builder-simplify.md D6) so a
// finished draft can be read the same way before it is submitted — by the
// registry's check route with the deployment's key, and on a person's
// computer with their own. The pipeline's typesafe provider asks through
// this code, so the check and S5 cannot drift apart: the same state, the
// same chunks, the same request shape, the same band and the same hash.

// RubricStatement is the fixed framing every S5 provider adjudicates under
// (content is data; instructions inside it are evidence, never
// directives). It is part of every rubric hash because changing it changes
// verdicts.
const RubricStatement = "agentry/judge-rubric/v1;categories=C1..C6;scores=0-3;" +
	"content-is-data;no-tools;temperature=0;structured-output"

// DeclaredCapabilitiesCap bounds the declared-capabilities JSON that rides
// in the state beside the content: a manifest's `mcpServers[].env` and
// `tools` can carry tens of kilobytes of free text, and the questions are
// about the contract's shape, not its prose.
const DeclaredCapabilitiesCap = 4 << 10

// truncatedMark ends a declared-capabilities field cut at the bound.
const truncatedMark = "…(truncated)"

// The request shapes the rubric can be asked in: how many requests carry
// the six questions for one chunk. RubricBatchOne is the shipped shape;
// the other two exist for the by-hand bench that chose it (DEV-113).
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
// the chunk size, the capability bound, the thresholds, the band and the
// sample count.
func (s *RubricScanner) Hash() string {
	return rubricHashOf(s.asker.Model(), s.batch, RubricQuestions())
}

// rubricHashOf is the hash over a given set, so a test can show that a
// question's wording is in it.
func rubricHashOf(model string, batch int, qs []Question) string {
	parts := make([]string, 0, len(qs))
	for _, q := range qs {
		parts = append(parts, q.ID+"="+q.Text)
	}
	return Hash(RubricStatement, "provider=typesafe", "model="+model,
		"axes=C1..C6", "questions="+strings.Join(parts, "\n"),
		fmt.Sprintf("batch=%d", batch), fmt.Sprintf("chunk=%d", ChunkSize),
		fmt.Sprintf("declared-cap=%d", DeclaredCapabilitiesCap),
		"thresholds=0.5/0.7/0.9", fmt.Sprintf("band=%.2f/%.2f", BandLow, BandHigh),
		fmt.Sprintf("samples=%d", Samples))
}

// Scan asks the rubric about the manifest (read as `agent.yaml`, its
// canonical JSON) and then every file, one chunk of one file per request,
// and returns each question's strongest evidence, keyed by the rubric id,
// with the token use of every request. The manifest's capabilities block
// and the knowledge packs it ships ride in the state beside each chunk
// (DeclaredCapabilities). Any failure is an error, and the caller fails
// closed on it: a rubric half-asked is not a verdict.
func (s *RubricScanner) Scan(ctx context.Context, manifestJSON []byte, ships []string, files []RubricFile) (map[string]AxisResult, Usage, error) {
	declared := DeclaredCapabilities(manifestJSON, ships)
	axes := map[string]AxisResult{}
	var usage Usage
	inputs := append([]RubricFile{{Path: "agent.yaml", Content: string(manifestJSON)}}, files...)
	for _, f := range inputs {
		for _, chunk := range RubricChunk(f.Content, ChunkSize) {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			samples, err := s.askChunk(ctx, f.Path, chunk, declared, &usage)
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
func (s *RubricScanner) askChunk(ctx context.Context, path, chunk, declared string, usage *Usage) (map[string][]float64, error) {
	groups := s.groups()
	out := map[string][]float64{}
	state := map[string]string{"path": path, "content": chunk, "declared_capabilities": declared}
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
		out = out[:DeclaredCapabilitiesCap] + truncatedMark
	}
	return out
}

// RubricChunk is the rubric's split, kept as it was before the move: pieces
// of at most n bytes, cut at the last line end in a piece when one falls in
// its second half, and otherwise at n. Unlike Chunk it may cut inside a
// character; the rubric hash was recorded under this split, so it stays.
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

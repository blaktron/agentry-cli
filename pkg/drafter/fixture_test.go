package drafter

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
)

// fixtures is the fixture set (plan D10): described tasks with the shapes
// expected of their drafts, the regression check on the instruction and the
// model. It is the tests' and scripts/drafter-fixtures.sh's, never part of
// the package a run is given.
//
//go:embed fixtures/*.json
var fixtures embed.FS

// Fixture is one described task of the fixture set (plan D10) and the shape
// expected of its draft.
type Fixture struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Hints       []string `json:"hints"`
	Expect      Expect   `json:"expect"`
}

// Expect is a draft's expected shape: what a fair reading of the
// description needs, and no more.
type Expect struct {
	// Modes are the run modes, exactly.
	Modes []string `json:"modes"`
	// Hosts is every host the draft may reach, its own egress and its
	// servers' together; none when empty. AnyHosts takes any exact hosts
	// (a task whose hosts only the person knows), never "*".
	Hosts    []string `json:"hosts,omitempty"`
	AnyHosts bool     `json:"anyHosts,omitempty"`
	// MaxSecrets and MaxServers bound the keys (its own and its servers')
	// and the declared MCP servers; MinSecrets is the fewest keys a fair
	// reading needs, for a description that names one.
	MaxSecrets int `json:"maxSecrets"`
	MinSecrets int `json:"minSecrets,omitempty"`
	MaxServers int `json:"maxServers"`
	// Exec and Write allow a command or a filesystem write.
	Exec  bool `json:"exec"`
	Write bool `json:"write"`
	// Intake wants at least one intake question; Open wants an intake
	// question or an open question, for a description that leaves
	// something only the person knows.
	Intake bool `json:"intake,omitempty"`
	Open   bool `json:"open,omitempty"`
	// InputKinds are kinds the draft's inputs block must declare, so the
	// block is required; InputViews are the views a declared block may
	// choose, checked only when there is one (agentry-cli#634). Neither is
	// checked when empty.
	InputKinds []string `json:"inputKinds,omitempty"`
	InputViews []string `json:"inputViews,omitempty"`
	// Keys is the marking the description calls for on every key a draft
	// declares: "optional" when the QuickAgent can do its job without it,
	// so each is in optionalSecrets, "required" when it cannot, so none is;
	// not checked when empty (plan optional-secrets.md D3, agentry-cli#652).
	Keys string `json:"keys,omitempty"`
	// Remote are hosts of vendors' remote MCP servers the draft must
	// declare by address, each signed in to by OAuth, and SignIns the
	// package servers (ecosystem:package) that must carry the sign-in the
	// guide lists for them (agentry-cli#667). Neither is checked when empty.
	Remote  []string `json:"remote,omitempty"`
	SignIns []string `json:"signIns,omitempty"`
}

// LoadFixtures reads the fixture set, sorted by id.
func LoadFixtures() ([]Fixture, error) {
	paths, err := fs.Glob(fixtures, "fixtures/*.json")
	if err != nil {
		return nil, err
	}
	var out []Fixture
	for _, p := range paths {
		b, err := fs.ReadFile(fixtures, p)
		if err != nil {
			return nil, err
		}
		var f Fixture
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// CheckShape lists how a draft's manifest departs from the shape expected;
// empty when it holds.
func CheckShape(m *manifest.Manifest, d *Draft, e Expect) []string {
	var out []string
	var modes []string
	for _, mode := range m.SupportedModes() {
		modes = append(modes, string(mode))
	}
	sort.Strings(modes)
	want := append([]string(nil), e.Modes...)
	sort.Strings(want)
	if !slices.Equal(modes, want) {
		out = append(out, fmt.Sprintf("modes %v, want %v", modes, want))
	}
	c := m.Capabilities
	hosts := append([]string(nil), c.Network.Egress.Hosts...)
	keys := append([]string(nil), c.Secrets...)
	remote := map[string]bool{}
	signIns := map[string]bool{}
	for _, s := range c.MCPServers {
		hosts = append(hosts, s.Egress...)
		keys = append(keys, s.Secrets...)
		if s.IsRemote() {
			// A remote server runs at its vendor: no version, and its one
			// host is its address's.
			hosts = append(hosts, s.RemoteHost())
			if s.Auth == manifest.AuthOAuth {
				remote[s.RemoteHost()] = true
			}
			continue
		}
		if l, err := serverList(); err == nil && s.OAuth != nil {
			eco, pkg := packageOf(s)
			if si, ok := l.SignIn(eco, pkg, s.Version); ok && si.Differs(s) == "" {
				signIns[s.SourceID()] = true
			}
		}
		if s.Version == "" || strings.ContainsAny(s.Version, "^~*<> ") || s.Version == "latest" {
			out = append(out, fmt.Sprintf("server %s is not pinned at an exact version (%q)", s.SourceID(), s.Version))
		}
		// The post-step's list check (agentry-cli#660) has had its say by
		// now; a pin it covers that is still here is the bench's to count.
		if l, err := serverList(); err == nil && !s.IsRemote() {
			eco, pkg := packageOf(s)
			if e, ok := l.NotRunHere(eco, pkg, s.Version); ok {
				out = append(out, fmt.Sprintf("server %s@%s is on Does not run here (%s)", s.SourceID(), s.Version, e.Reason))
			}
		}
	}
	for _, h := range hosts {
		switch {
		case strings.Contains(h, "*"):
			out = append(out, fmt.Sprintf("egress %q is a wildcard", h))
		case !e.AnyHosts && !slices.Contains(e.Hosts, strings.ToLower(h)):
			out = append(out, fmt.Sprintf("reaches %s, not among %v", h, e.Hosts))
		}
	}
	if n := len(keys); n > e.MaxSecrets {
		out = append(out, fmt.Sprintf("%d keys, want at most %d", n, e.MaxSecrets))
	} else if n < e.MinSecrets {
		out = append(out, fmt.Sprintf("%d keys, want at least %d", n, e.MinSecrets))
	}
	for _, k := range keys {
		switch optional := c.IsOptionalSecret(k); {
		case e.Keys == "optional" && !optional:
			out = append(out, fmt.Sprintf("key %s is required; the QuickAgent can do its job without it", k))
		case e.Keys == "required" && optional:
			out = append(out, fmt.Sprintf("key %s is optional; the QuickAgent cannot do its job without it", k))
		}
	}
	for _, h := range e.Remote {
		if !remote[h] {
			out = append(out, fmt.Sprintf("no remote server at %s signed in to by OAuth", h))
		}
	}
	for _, id := range e.SignIns {
		if !signIns[id] {
			out = append(out, fmt.Sprintf("%s does not carry the sign-in the drafting guide lists", id))
		}
	}
	if n := len(c.MCPServers); n > e.MaxServers {
		out = append(out, fmt.Sprintf("%d MCP servers, want at most %d", n, e.MaxServers))
	}
	if !e.Exec && !c.Exec.IsZero() && !c.Exec.None {
		out = append(out, fmt.Sprintf("declares exec %v", c.Exec.Commands))
	}
	if !e.Write && c.Filesystem.Write.Mode != "" && c.Filesystem.Write.Mode != manifest.FSModeNone {
		out = append(out, fmt.Sprintf("declares a filesystem write (%s)", c.Filesystem.Write.Mode))
	}
	if m.Inputs == nil && len(e.InputKinds) > 0 {
		out = append(out, "no inputs block")
	}
	if m.Inputs != nil {
		for _, k := range e.InputKinds {
			if !slices.Contains(m.Inputs.Kinds, manifest.InputKind(k)) {
				out = append(out, fmt.Sprintf("inputs.kinds %v, want %s among them", m.Inputs.Kinds, k))
			}
		}
		if len(e.InputViews) > 0 && !slices.Contains(e.InputViews, string(m.Inputs.View)) {
			out = append(out, fmt.Sprintf("inputs.view %q, want one of %v", m.Inputs.View, e.InputViews))
		}
	}
	if e.Intake && len(m.Intake) == 0 {
		out = append(out, "no intake question")
	}
	if e.Open && len(m.Intake) == 0 && len(d.OpenQuestions) == 0 {
		out = append(out, "neither an intake question nor an open question")
	}
	if strings.TrimSpace(d.Instructions) == "" {
		out = append(out, "no instructions")
	} else if !inOrder(d.Instructions, instructionShape) {
		out = append(out, "the instructions are not in the shape Role, Inputs, Steps, Output, Background, Checks")
	}
	if n := len(d.Instructions); n > MaxInstructionBytes {
		out = append(out, fmt.Sprintf("instructions of %d bytes, want at most %d", n, MaxInstructionBytes))
	}
	// The Drafter's own caps (plan builder-simplify.md §5.1, D8).
	if n := len(m.Intake); n > MaxIntake {
		out = append(out, fmt.Sprintf("%d intake questions, want at most %d", n, MaxIntake))
	}
	if m.Guide != nil && len(m.Guide.Prepare) > MaxPrepare {
		out = append(out, fmt.Sprintf("%d prepare items, want at most %d", len(m.Guide.Prepare), MaxPrepare))
	}
	if n := len(d.OpenQuestions); n > MaxOpenQuestions {
		out = append(out, fmt.Sprintf("%d open questions, want at most %d", n, MaxOpenQuestions))
	}
	if n := noteBullets(d.Notes); n > MaxNoteBullets {
		out = append(out, fmt.Sprintf("a note of %d bullets, want at most %d", n, MaxNoteBullets))
	}
	return out
}

// instructionShape is the generated AGENT.md's fixed shape (plan
// builder-simplify.md §5.1; Background and Checks since agentry-cli#521,
// plan knowledge-and-drafter.md D5): its six headings, in this order.
var instructionShape = []string{"## Role", "## Inputs", "## Steps", "## Output", "## Background", "## Checks"}

// inOrder says every heading is a line of text, in order.
func inOrder(text string, headings []string) bool {
	at := 0
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		if at < len(headings) && strings.TrimSpace(l) == headings[at] {
			at++
		}
	}
	return at == len(headings)
}

// postStepSections open the sections the post-step adds to the notes.
var postStepSections = []string{"## Corrected after drafting", "## Repaired once", "## Not repaired", "## Withheld by AI Review"}

// noteBullets counts the list items of the notes the Drafter wrote, before
// the post-step's own sections.
func noteBullets(notes string) int {
	for _, h := range postStepSections {
		if i := strings.Index(notes, h); i >= 0 {
			notes = notes[:i]
		}
	}
	n := 0
	for _, l := range strings.Split(notes, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* ") || (len(l) > 2 && l[0] >= '0' && l[0] <= '9' && strings.HasPrefix(l[1:], ". ")) {
			n++
		}
	}
	return n
}

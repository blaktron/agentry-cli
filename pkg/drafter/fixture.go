package drafter

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/blaktron/agentry/pkg/manifest"
)

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
	// and the declared MCP servers.
	MaxSecrets int `json:"maxSecrets"`
	MaxServers int `json:"maxServers"`
	// Exec and Write allow a command or a filesystem write.
	Exec  bool `json:"exec"`
	Write bool `json:"write"`
	// Intake wants at least one intake question; Open wants an intake
	// question or an open question, for a description that leaves
	// something only the person knows.
	Intake bool `json:"intake,omitempty"`
	Open   bool `json:"open,omitempty"`
}

// LoadFixtures reads the fixture set, sorted by id.
func LoadFixtures() ([]Fixture, error) {
	paths, err := fs.Glob(Fixtures(), "fixtures/*.json")
	if err != nil {
		return nil, err
	}
	var out []Fixture
	for _, p := range paths {
		b, err := fs.ReadFile(Fixtures(), p)
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
	secrets := len(c.Secrets)
	for _, s := range c.MCPServers {
		hosts = append(hosts, s.Egress...)
		secrets += len(s.Secrets)
		if s.Version == "" || strings.ContainsAny(s.Version, "^~*<> ") || s.Version == "latest" {
			out = append(out, fmt.Sprintf("server %s is not pinned at an exact version (%q)", s.SourceID(), s.Version))
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
	if secrets > e.MaxSecrets {
		out = append(out, fmt.Sprintf("%d keys, want at most %d", secrets, e.MaxSecrets))
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
	if e.Intake && len(m.Intake) == 0 {
		out = append(out, "no intake question")
	}
	if e.Open && len(m.Intake) == 0 && len(d.OpenQuestions) == 0 {
		out = append(out, "neither an intake question nor an open question")
	}
	if strings.TrimSpace(d.Instructions) == "" {
		out = append(out, "no instructions")
	}
	return out
}

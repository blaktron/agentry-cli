package mcpinstall

import (
	"strings"
	"testing"
)

// RuntimeMismatch admits what npm and pip would install on the runtime and
// names what they would not (agentry-cli#609).
func TestRuntimeMismatch(t *testing.T) {
	for _, c := range []struct {
		source, runtime string
		want            string // "" admits; else a substring of the reason
	}{
		{"npm", "", ""},
		{"npm", ">=24.0.0", ""},
		{"npm", ">=18.0.0 <99", ""},
		{"npm", "^20 || >=22", ""},
		{"npm", "^18.17.0 || >=20.5.0", ""},
		{"npm", ">=26", "needs Node >=26 (its engines.node), and runs use Node 24.21.0"},
		{"npm", "^22", "needs Node ^22"},
		{"npm", "20.x || 22.x", "needs Node"},
		{"npm", "not a range", "not a range npm can read"},
		{"pypi", "", ""},
		{"pypi", ">=3.10", ""},
		{"pypi", ">=3.8,<4", ""},
		{"pypi", "~=3.12", ""},
		{"pypi", "==3.12.*", ""},
		{"pypi", ">=3.10, !=3.11.*", ""},
		{"pypi", ">=3.13", "needs Python >=3.13 (its requires_python), and runs use Python 3.12.14"},
		{"pypi", "~=3.11.0", "needs Python"},
		{"pypi", "!=3.12.*", "needs Python"},
		{"pypi", "<3.12", "needs Python"},
		{"pypi", "python3", "not a specifier pip can read"},
	} {
		got := RuntimeMismatch(Resolved{Source: c.source, Runtime: c.runtime}, "24.21.0", "3.12.14")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s %q: got %q, want %q", c.source, c.runtime, got, c.want)
		}
	}
}

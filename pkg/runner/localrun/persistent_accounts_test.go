package localrun

import (
	"errors"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// TestGitHostAccountsComeOnlyWithTheCodingToolchain: a segment is given git
// host accounts only when its release declares capabilities.persistent.coder
// and the roster has the toolchain's commands on, whatever else it is
// given; stored secrets only when it declares secrets.
func TestGitHostAccountsComeOnlyWithTheCodingToolchain(t *testing.T) {
	parse := func(keys string) *manifest.Manifest {
		t.Helper()
		m, err := manifest.Parse([]byte("schemaVersion: \"2\"\nname: helper\nnamespace: acme\nversion: 1.0.0\nsummary: s\nlicense: MIT\nmodes: [persistent]\ncapabilities:\n  persistent:\n" + keys))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	account := []Account{{CoderAccount: spec.CoderAccount{Host: "github.com", User: "ada"}, Token: "t"}}
	commands := &spec.CoderGrant{Commands: true}
	for _, c := range []struct {
		name  string
		keys  string
		g     Grants
		wants string
	}{
		{"accounts without coder declared", "    clock: true\n    secrets: [A]\n", Grants{Accounts: account, Coder: commands}, "git host accounts"},
		{"accounts with coder declared and no commands", "    coder: true\n", Grants{Accounts: account, Coder: &spec.CoderGrant{}}, "git host accounts"},
		{"accounts with coder declared and no toolchain", "    coder: true\n", Grants{Accounts: account}, "git host accounts"},
		{"stored secrets without secrets declared", "    coder: true\n", Grants{Secrets: map[string]string{"A": "a"}}, "stored secrets"},
		{"withheld stored secrets without secrets declared", "    clock: true\n", Grants{WithheldSecrets: []string{"A"}}, "stored secrets"},
		{"accounts with the toolchain's commands", "    coder: true\n", Grants{Accounts: account, Coder: commands}, ""},
		{"stored secrets declared", "    secrets: [A]\n", Grants{Secrets: map[string]string{"A": "a"}}, ""},
	} {
		err := Segment{Grants: c.g}.checkSecrets(parse(c.keys))
		if c.wants == "" && err != nil || c.wants != "" && (err == nil || !strings.Contains(err.Error(), c.wants)) {
			t.Errorf("%s: %v", c.name, err)
		}
		// A refusal is the policy's.
		var ce *engine.ClassifiedError
		if c.wants != "" && (!errors.As(err, &ce) || ce.Code != engine.ExitPolicy) {
			t.Errorf("%s: %v is not a policy refusal", c.name, err)
		}
	}
}

package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/installed"
)

// Optional secrets in the CLI's own surfaces (DR-70, agentry-cli#651).

// TestDoctorAnOptionalSecretUnsetIsANote: doctor fails only on a required
// secret that would not resolve; an optional one is a note in an ok check.
func TestDoctorAnOptionalSecretUnsetIsANote(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	dir := writeTree(t, skillFiles("optional-secret", "capabilities:\n  secrets: [GITHUB_PAT, REQUIRED_PAT]\n  optionalSecrets: [GITHUB_PAT]\n"))
	t.Setenv("GITHUB_PAT", "")
	t.Setenv("REQUIRED_PAT", "required-value-set")

	check := func() doctorCheck {
		out, _, _ := runCLI(t, nil, "doctor", "--local", dir, "--registry", fr.url(), "--policy", policyPath, "--json")
		var rep doctorReport
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("json: %v\n%s", err, out)
		}
		for _, c := range rep.Checks {
			if c.Name == "secrets" {
				return c
			}
		}
		t.Fatalf("no secrets check:\n%s", out)
		return doctorCheck{}
	}
	if c := check(); c.Status != "ok" || !strings.Contains(c.Detail, "optional and not set, so a run goes without them: GITHUB_PAT") {
		t.Fatalf("an unset optional secret: %+v", c)
	}
	t.Setenv("REQUIRED_PAT", "")
	c := check()
	if c.Status != "fail" || !strings.Contains(c.Detail, "would not resolve (run refuses with exit 11): REQUIRED_PAT —") {
		t.Fatalf("an unset required secret: %+v", c)
	}
}

// TestTheConsentNamesEveryDeclaredSecret is the condition DEV-212 puts on
// plan D6: the consent lines and the install snapshot name every declared
// secret whatever its marking, so a later flip between optional and
// required asks nothing for a name the person never saw.
func TestTheConsentNamesEveryDeclaredSecret(t *testing.T) {
	caps := parseCaps(t, `capabilities:
  secrets: [REQUIRED_PAT, GITHUB_PAT]
  optionalSecrets: [GITHUB_PAT, SEARCH_KEY]
  mcpServers:
    - npm: some-search-server
      version: 1.2.3
      secrets: [SEARCH_KEY]
`)
	lines := capabilityLines(caps)
	for _, want := range []string{
		"secrets: REQUIRED_PAT, GITHUB_PAT",
		"optionalSecrets: GITHUB_PAT, SEARCH_KEY",
		"mcpServer: npm:some-search-server@1.2.3 (secrets: SEARCH_KEY)",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("the consent lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	if snap := installed.Snapshot(caps); !slices.Equal(snap.Secrets, []string{"REQUIRED_PAT", "GITHUB_PAT"}) {
		t.Errorf("the snapshot does not hold every declared secret: %v", snap.Secrets)
	}
	// Without the field the consent is the lines it always was.
	caps.OptionalSecrets = nil
	for _, l := range capabilityLines(caps) {
		if strings.HasPrefix(l, "optionalSecrets") {
			t.Errorf("a release with no optional secret shows %q", l)
		}
	}
}

// TestDoctorCountsTheRequiredSecretsAlone: with one required secret, one
// optional set and one optional unset, the ok line counts one required.
func TestDoctorCountsTheRequiredSecretsAlone(t *testing.T) {
	policyPath, _ := doctorFixture(t)
	fr := newFakeRegistry(t)
	dir := writeTree(t, skillFiles("optional-count", "capabilities:\n  secrets: [REQUIRED_PAT, SET_PAT, UNSET_PAT]\n  optionalSecrets: [SET_PAT, UNSET_PAT]\n"))
	t.Setenv("REQUIRED_PAT", "required-value-set")
	t.Setenv("SET_PAT", "optional-value-set")
	t.Setenv("UNSET_PAT", "")
	out, _, _ := runCLI(t, nil, "doctor", "--local", dir, "--registry", fr.url(), "--policy", policyPath)
	if !strings.Contains(out, "all 1 required secret(s) resolve; optional and not set, so a run goes without them: UNSET_PAT") {
		t.Fatalf("report:\n%s", out)
	}
}

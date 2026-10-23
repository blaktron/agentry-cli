package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// A persistent agent's stored secrets reach the engine as SecretOverrides
// (those on) and WithheldSecrets (those off).

const storedValue = "stored-value-31415926"

// TestAStoredSecretReachesItsServerSlotAndTheCodersPrograms: a value on
// fills a declared server's slot ahead of the policy source, and the
// Coding agent's programs' key from the segment's values alone.
func TestAStoredSecretReachesItsServerSlotAndTheCodersPrograms(t *testing.T) {
	p, _ := fixture(t)
	p.Plan.MCPServers = []spec.MCPServerSpec{{Name: "srv", Secrets: []string{"STORED_TOKEN"}}}
	p.Plan.Capabilities.Coder = &spec.CoderGrant{Commands: true, Secrets: []string{"STORED_TOKEN"}}
	t.Setenv("STORED_TOKEN", "from-the-environment")
	p.SecretOverrides = map[string]string{"STORED_TOKEN": storedValue}
	values, err := secretValues(p)
	if err != nil {
		t.Fatal(err)
	}
	if values[compile.ServerSecretKey("srv", "STORED_TOKEN")] != storedValue || values[compile.SecretKey("STORED_TOKEN")] != storedValue {
		t.Fatalf("the stored value did not reach both slots: %v", values)
	}
	// The programs' key is never read from the policy source.
	p.SecretOverrides = nil
	if _, err := secretValues(p); err == nil || !strings.Contains(err.Error(), "STORED_TOKEN") {
		t.Fatalf("a coder secret with no stored value: %v", err)
	}
	// Commands off: the programs have none.
	p.Plan.Capabilities.Coder.Commands = false
	p.SecretOverrides = map[string]string{"STORED_TOKEN": storedValue}
	values, err = secretValues(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values[compile.SecretKey("STORED_TOKEN")]; ok {
		t.Fatalf("the programs were given a secret with commands off: %v", values)
	}
}

// TestAWithheldSecretIsWrittenEmptyAndNotReadFromThePolicy: a stored
// secret switched off is withheld from a server that declares it, even
// when the policy's source holds a value of that name, and even when the
// server requires it; the receipt names it.
func TestAWithheldSecretIsWrittenEmptyAndNotReadFromThePolicy(t *testing.T) {
	p, _ := fixture(t)
	p.Plan.MCPServers = []spec.MCPServerSpec{{Name: "srv", Secrets: []string{"STORED_TOKEN"}}}
	t.Setenv("STORED_TOKEN", "from-the-environment")
	p.WithheldSecrets = []string{"STORED_TOKEN", "NOT_DECLARED"}
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := writeSecretsFile(p, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), compile.ServerSecretKey("srv", "STORED_TOKEN")+"=\n") || strings.Contains(string(b), "from-the-environment") {
		t.Fatalf("the secrets file:\n%s", b)
	}
	if got := withheldSecrets(p); len(got) != 1 || got[0] != "STORED_TOKEN" {
		t.Fatalf("withheld = %v, want the declared one alone", got)
	}
	if got := withWithheldNote(nil, withheldSecrets(p))["secrets.withheld"]; !strings.HasSuffix(got, ": STORED_TOKEN") {
		t.Fatalf("the receipt's line: %q", got)
	}
}

// TestSecretValueIsMaskedEverywhere: a segment given a stored secret masks
// its value wherever the segment writes what the agent did: the events, the
// transcript, every receipt file and the result. The fake harness echoes the
// turn's text, which here holds the value, as a server's result would.
func TestSecretValueIsMaskedEverywhere(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	p.Plan.MCPServers = []spec.MCPServerSpec{{Name: "srv", Secrets: []string{"STORED_TOKEN"}}}
	p.SecretOverrides = map[string]string{"STORED_TOKEN": storedValue}
	p.ServersDir = t.TempDir()
	f.LogsByName = map[string]string{"agentry-5e9e0001-gateway": "- srv: auth with " + storedValue + "\n    - STORED_TOKEN: " + storedValue[:4] + "****\n"}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	if err := s.Send(context.Background(), "the server answered "+storedValue); err != nil {
		t.Fatal(err)
	}
	env, err := s.End(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	for _, ev := range get() {
		if strings.Contains(ev.Text, storedValue) {
			t.Errorf("an event holds the value: %+v", ev)
		}
	}
	seen := 0
	err = filepath.Walk(p.OutDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // the segment's own output
		if err != nil {
			return err
		}
		seen++
		if strings.Contains(string(b), storedValue) || strings.Contains(string(b), storedValue[:4]+"****") {
			return errors.New(path + " holds the value")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 3 {
		t.Fatalf("the segment wrote %d files", seen)
	}
	tr, err := os.ReadFile(filepath.Join(env.Receipts.Dir, "transcript.md"))
	if err != nil || !strings.Contains(string(tr), "[redacted]") {
		t.Fatalf("the transcript does not show the mask: %v\n%s", err, tr)
	}
}

const accountToken = "ghp_accounttoken27182818"

// coderWithAccount is a Coding agent's grant with its commands on and an
// account on github.com, whose token is the segment's.
func coderWithAccount(p *Params) {
	p.Plan.Capabilities.Coder = &spec.CoderGrant{Commands: true, Accounts: []spec.CoderAccount{
		{Host: "github.com", Kind: spec.GitHostGitHub, Own: true, User: "coder-bot", Name: "Coder Bot", Email: "bot@example.com", TokenEnv: spec.AccountTokenEnv(0)},
	}}
	p.AccountTokens = map[string]string{spec.AccountTokenEnv(0): accountToken}
}

// TestAnAccountsTokenReachesTheToolsContainerAlone: a git host account's
// token is written under the tools server's key from the segment's tokens
// alone, never a server's slot or the policy source; with no token the
// segment is refused, and with its commands off it is given none.
func TestAnAccountsTokenReachesTheToolsContainerAlone(t *testing.T) {
	p, _ := fixture(t)
	coderWithAccount(&p)
	t.Setenv(spec.AccountTokenEnv(0), "from-the-environment")
	values, err := secretValues(p)
	if err != nil {
		t.Fatal(err)
	}
	holding := 0
	for _, v := range values {
		if v == accountToken {
			holding++
		}
	}
	if values[compile.SecretKey(spec.AccountTokenEnv(0))] != accountToken || holding != 1 {
		t.Fatalf("values = %v", values)
	}
	p.AccountTokens = nil
	if _, err := secretValues(p); err == nil || !strings.Contains(err.Error(), "the account on github.com") {
		t.Fatalf("an account with no token: %v", err)
	}
	p.Plan.Capabilities.Coder.Commands = false
	p.AccountTokens = map[string]string{spec.AccountTokenEnv(0): accountToken}
	if values, err := secretValues(p); err != nil || values[compile.SecretKey(spec.AccountTokenEnv(0))] != "" {
		t.Fatalf("commands off: %v, %v", values, err)
	}
}

// TestAnAccountsTokenIsMaskedEverywhere: as a stored secret's value, the
// token is masked in the events, the transcript, every receipt file and
// the result.
func TestAnAccountsTokenIsMaskedEverywhere(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	coderWithAccount(&p)
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	if err := s.Send(context.Background(), "the program printed "+accountToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.End(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-done
	for _, ev := range get() {
		if strings.Contains(ev.Text, accountToken) {
			t.Errorf("an event holds the token: %+v", ev)
		}
	}
	seen := 0
	err = filepath.Walk(p.OutDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // the segment's own output
		if err != nil {
			return err
		}
		seen++
		if strings.Contains(string(b), accountToken) {
			return errors.New(path + " holds the token")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 3 {
		t.Fatalf("the segment wrote %d files", seen)
	}
}

// TestADependencyDatabaseIsGivenExactlyWhenThePlanScans: the folder of
// OSV's databases is mounted for a plan whose Coding agent scans, as a real
// folder, and refused for one that does not, or given through a link.
func TestADependencyDatabaseIsGivenExactlyWhenThePlanScans(t *testing.T) {
	p, _ := fixture(t)
	p.Persistent = &PersistentSegment{}
	dir := t.TempDir()
	scans := &spec.CoderGrant{Commands: true, DependencyScan: true}
	for name, tc := range map[string]struct {
		grant  *spec.CoderGrant
		dir    string
		wantOK bool
	}{
		"a scan with its folder":    {scans, dir, true},
		"a scan with none":          {scans, "", false},
		"a folder with no scan":     {&spec.CoderGrant{Commands: true}, dir, false},
		"neither":                   {nil, "", true},
		"a relative folder":         {scans, "osv", false},
		"a file":                    {scans, filepath.Join(dir, "f"), false},
		"its programs off, no scan": {&spec.CoderGrant{DependencyScan: true}, "", true},
	} {
		if name == "a file" {
			if err := os.WriteFile(tc.dir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		p.Plan.Capabilities.Coder = tc.grant
		p.Persistent.DependencyDB = tc.dir
		rd := &runDirs{}
		err := prepareDependencyDB(p, rd)
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: %v", name, err)
		}
		if err == nil && rd.dependencyDB != tc.dir {
			t.Errorf("%s: mounted %q", name, rd.dependencyDB)
		}
	}
}

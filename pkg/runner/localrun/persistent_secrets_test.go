package localrun

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/coder"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestASegmentGivesAStoredSecretToTheRosterServerThatDeclaresIt: the
// Assistant declares FASTMAIL_TOKEN as a stored secret, and Fastmail's
// offered server declares it as the secret it is given. With the server on
// and the secret stored, the segment's plan compiles: the server is the
// roster's, so its secret is the stored one, not a second secret the
// release asks for at each run.
func TestASegmentGivesAStoredSecretToTheRosterServerThatDeclaresIt(t *testing.T) {
	seg := Segment{Builtin: Assistant(), Home: t.TempDir(), Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: Grants{
		Servers:    []manifest.MCPServer{assistant.Fastmail().Server},
		Secrets:    map[string]string{assistant.FastmailToken: "tok"},
		ServersDir: t.TempDir(),
	}}
	if _, err := seg.Plan(context.Background(), nil); err != nil {
		t.Fatalf("plan: %v", err)
	}
}

// TestEveryBuiltinStartsWithEveryServerItOffersOn: each persistent built-in,
// as it ships, with every server it offers or suggests on its roster passes
// the manifest check a segment's start makes. A new version of a built-in,
// or a new offered server, that a start would refuse fails here.
func TestEveryBuiltinStartsWithEveryServerItOffersOn(t *testing.T) {
	hosts := []offer.GitHost{{Host: "github.com", Kind: "github"}, {Host: "gitlab.com", Kind: "gitlab"}, {Host: "git.example.com", Kind: "gitlab"}}
	for _, tc := range []struct {
		b         Builtin
		suggested []offer.Suggestion
	}{
		{Assistant(), assistant.SuggestedServers()},
		{Coder(), coder.SuggestedServers()},
		{Main(), nil},
	} {
		t.Run(tc.b.Ref(), func(t *testing.T) {
			raw, err := fs.ReadFile(tc.b.fsys, "agent.yaml")
			if err != nil {
				t.Fatal(err)
			}
			m, err := manifest.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			var roster []manifest.MCPServer
			for _, o := range tc.b.OfferedServers(hosts) {
				roster = append(roster, o.Server)
			}
			for _, s := range tc.suggested {
				roster = append(roster, s.Server)
			}
			if err := (Segment{Builtin: tc.b, Grants: Grants{Servers: roster}}).checkManifest(tc.b.Ref(), m); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestASegmentStillRefusesARunSecretOfTheRelease: a release that asks for a
// name at each run and also declares it as a stored secret is refused as it
// ships, whatever its roster holds.
func TestASegmentStillRefusesARunSecretOfTheRelease(t *testing.T) {
	m := &manifest.Manifest{SchemaVersion: "2", Name: "a", Namespace: "n", Version: "1.0.0", Summary: "s",
		Modes:       []manifest.Mode{manifest.ModePersistent},
		Entrypoints: manifest.Entrypoints{Instruction: "INSTRUCTION.md"},
		Capabilities: manifest.Capabilities{
			Secrets:    []string{"API_KEY"},
			Persistent: &manifest.Persistent{Secrets: []string{"API_KEY"}},
		}}
	roster := []manifest.MCPServer{{URL: "https://mcp.example.com/mcp", Auth: manifest.AuthSecret, Secrets: []string{"API_KEY"}}}
	err := (Segment{Grants: Grants{Servers: roster}}).checkManifest("n/a@1.0.0", m)
	if err == nil || !strings.Contains(err.Error(), `"API_KEY" is also a secret the release asks for at each run`) {
		t.Fatalf("want the V16 refusal, got %v", err)
	}
}

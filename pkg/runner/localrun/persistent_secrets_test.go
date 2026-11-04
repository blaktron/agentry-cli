package localrun

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/persistent/coder"
	"github.com/blaktron/agentry-cli/pkg/persistent/offer"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
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
		offered   []offer.Server
	}{
		{Assistant(), assistant.SuggestedServers(), assistant.OfferedServers()},
		{Coder(), coder.SuggestedServers(), coder.OfferedServers(hosts)},
		{Main(), nil, nil},
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
			// A built-in is offered its own package's servers and suggested its
			// own package's, and Agentry Helper none.
			if got := tc.b.OfferedServers(hosts); len(got) != len(tc.offered) || !reflect.DeepEqual(got, tc.offered) || (tc.b.Persistent() && tc.b.Ref() != Main().Ref() && len(got) == 0) {
				t.Fatalf("%s is offered %+v, want %+v", tc.b.Ref(), got, tc.offered)
			}
			if got := tc.b.SuggestedServers(); !reflect.DeepEqual(got, tc.suggested) {
				t.Fatalf("%s is suggested %+v, want %+v", tc.b.Ref(), got, tc.suggested)
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

	// A server the roster adds is held to the manifest rules beside the release's
	// own: one they refuse refuses the segment, naming the rule and the server; one
	// they only warn of does not.
	raw, rerr := fs.ReadFile(Assistant().fsys, "agent.yaml")
	if rerr != nil {
		t.Fatal(rerr)
	}
	shipped, rerr := manifest.Parse(raw)
	if rerr != nil {
		t.Fatal(rerr)
	}
	insecure := []manifest.MCPServer{{URL: "http://mcp.example.com/mcp"}}
	err = (Segment{Grants: Grants{Servers: insecure}}).checkManifest(Assistant().Ref(), shipped)
	var ce *engine.ClassifiedError
	if !errors.As(err, &ce) || ce.Code != engine.ExitCompile || !strings.Contains(err.Error(), Assistant().Ref()+" manifest invalid") ||
		!strings.Contains(err.Error(), "manifest V5: capabilities.mcpServers[") || !strings.Contains(err.Error(), "must be https://") {
		t.Errorf("a roster server over plain http: %v", err)
	}
	wide := []manifest.MCPServer{{NPM: "@scope/wide", Version: "1.0.0", Egress: []string{"*"}}}
	if err := (Segment{Grants: Grants{Servers: wide}}).checkManifest(Assistant().Ref(), shipped); err != nil {
		t.Errorf("a roster server the rules only warn of: %v", err)
	}
}

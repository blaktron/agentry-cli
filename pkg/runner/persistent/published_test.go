package persistent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/installdir"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/trust"
)

// publishedRelease is a persistent release's tree: its manifest with extra
// lines, its instruction, and a persona file when persona is not empty.
type publishedRelease struct {
	extra   string
	persona string
	// pack ships the Drafter's guide as the release's own pack.
	pack bool
}

// installPublished lays a persistent release of ns/name@version down in the
// runner home as a verified registry install, sealed under the home's key,
// and records it as a published agent.
func installPublished(t *testing.T, home, ns, name, version string, r publishedRelease) Agent {
	t.Helper()
	tree := t.TempDir()
	text := "schemaVersion: \"2\"\nname: " + name + "\nnamespace: " + ns + "\nmodes: [persistent]\nversion: " + version + "\nsummary: s\n" + r.extra
	include := "agent.yaml, AGENT.md"
	if r.persona != "" {
		include += ", PERSONA.md"
		if err := os.WriteFile(filepath.Join(tree, "PERSONA.md"), []byte(r.persona), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if r.pack {
		text += "knowledge:\n  ships:\n    - name: drafting-guide\n"
		include += `, "knowledge/**"`
		sub, err := fs.Sub(drafter.FS(), "knowledge/drafting-guide")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(filepath.Join(tree, "knowledge", "drafting-guide"), sub); err != nil {
			t.Fatal(err)
		}
	}
	text += "publish:\n  include: [" + include + "]\n"
	for f, body := range map[string]string{"agent.yaml": text, "AGENT.md": "You keep notes for the person.\n"} {
		if err := os.WriteFile(filepath.Join(tree, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := localrun.TreeDigest(tree)
	if err != nil {
		t.Fatal(err)
	}
	meta := &installdir.Meta{Ref: ns + "/" + name + "@" + version, Namespace: ns, Name: name, Version: version, Digest: digest,
		Source: SourceRegistry, Attestation: receipt.Attestation{State: trust.StateVerified, Verified: true}}
	if err := installdir.Write(installdir.ReleaseDir(home, ns, name, version, digest), tree, meta); err != nil {
		t.Fatal(err)
	}
	a, err := SaveRelease(home, Release{Ref: ns + "/" + name, Version: version, Digest: digest, Source: SourceRegistry})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestTwoRefsSameNameShareNothing: two publishers' agents of one name are
// two agents, each keyed by its reference, with a state folder, volumes, a
// socket and an owner lock of its own; a reference names each one, and the
// name they share names neither, the refusal naming both.
func TestTwoRefsSameNameShareNothing(t *testing.T) {
	home := shortHome(t)
	alice := installPublished(t, home, "alice", "helper", "1.0.0", publishedRelease{})
	bob := installPublished(t, home, "bob", "helper", "2.1.0", publishedRelease{})
	if alice.Name == bob.Name {
		t.Fatalf("both are keyed %q", alice.Name)
	}
	for _, f := range []func(string, string) string{Dir, SocketPath, AppSocketPath, SessionVolumeIn, DataVolumeIn, MemoryVolumeIn} {
		if a, b := f(home, alice.Name), f(home, bob.Name); a == b || strings.Contains(a, "/helper") || strings.Contains(b, "/helper") {
			t.Errorf("alice %q, bob %q: want each its own, keyed by its reference", a, b)
		}
	}
	for _, a := range []Agent{alice, bob} {
		if !strings.HasPrefix(Dir(home, a.Name), filepath.Join(home, "persistent")+string(filepath.Separator)) {
			t.Errorf("%s's folder %s is not under the runner home's persistent folder", a.Ref, Dir(home, a.Name))
		}
		for _, b := range Agents() {
			if strings.HasPrefix(Dir(home, a.Name), Dir(home, b.Name)+string(filepath.Separator)) || a.Name == b.Name {
				t.Errorf("%s's folder is inside the built-in %s's", a.Ref, b.Name)
			}
		}
	}
	owner, err := Acquire(home, alice.Name, ClientCLI)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	if held, _ := Held(home, bob.Name); held {
		t.Error("holding alice/helper holds bob/helper")
	}
	cfg := Config{Version: ConfigVersion, Enabled: true, DisplayName: "Alice's"}
	if err := SaveConfig(home, alice.Name, cfg); err != nil {
		t.Fatal(err)
	}
	if c, _ := LoadConfig(home, bob.Name); c.DisplayName != "" {
		t.Errorf("bob/helper reads alice/helper's configuration: %+v", c)
	}
	for ref, want := range map[string]string{"alice/helper": "1.0.0", "bob/helper": "2.1.0", alice.Name: "1.0.0"} {
		got, err := LookupIn(home, ref)
		if err != nil || got.Version != want {
			t.Errorf("LookupIn(%q) = %+v, %v; want version %s", ref, got, err, want)
		}
	}
	if _, err := LookupIn(home, "helper"); err == nil || !strings.Contains(err.Error(), "alice/helper") || !strings.Contains(err.Error(), "bob/helper") {
		t.Errorf("LookupIn(helper) = %v, want a refusal naming both", err)
	}
	listed := AgentsIn(home)
	if len(listed) != len(Agents())+2 || listed[len(listed)-2].Ref != "alice/helper" || listed[len(listed)-1].Ref != "bob/helper" {
		t.Errorf("AgentsIn = %v", listed)
	}
}

// TestAShortNameNamesAPublishedAgentUnlessABuiltinHasIt: a published agent's
// name alone names it when no other agent has that name; a built-in's short
// name always names the built-in, and the published agent of that name is
// named by its reference. A built-in's own reference is never installed as
// a published agent.
func TestAShortNameNamesAPublishedAgentUnlessABuiltinHasIt(t *testing.T) {
	home := shortHome(t)
	installPublished(t, home, "acme", "notes", "1.0.0", publishedRelease{})
	installPublished(t, home, "acme", "assistant", "1.0.0", publishedRelease{})
	if a, err := LookupIn(home, "notes"); err != nil || a.Ref != "acme/notes" {
		t.Errorf("notes = %+v, %v", a, err)
	}
	if a, err := LookupIn(home, "assistant"); err != nil || a.Published() {
		t.Errorf("assistant = %+v, %v; want the built-in", a, err)
	}
	if a, err := LookupIn(home, "acme/assistant"); err != nil || !a.Published() {
		t.Errorf("acme/assistant = %+v, %v; want the published agent", a, err)
	}
	if a, err := LookupIn(home, "agentry/assistant"); err != nil || a.Published() {
		t.Errorf("agentry/assistant = %+v, %v; want the built-in", a, err)
	}
	if _, err := SaveRelease(home, Release{Ref: "agentry/coder", Version: "9.9.9", Digest: "sha256:x", Source: SourceRegistry}); err == nil {
		t.Error("a built-in's reference was installed as a published agent")
	}
	if _, err := LookupIn(home, "nobody"); err == nil || !strings.Contains(err.Error(), "acme/notes") {
		t.Errorf("an unknown name: %v, want the agents listed", err)
	}
}

// TestUninstallKeepsDataUnlessAsked: uninstalling a published agent takes it
// off the agents and keeps its folder and volumes, which installing it again
// takes up; with removeData both go. A built-in is never uninstalled, and a
// running agent is refused.
func TestUninstallKeepsDataUnlessAsked(t *testing.T) {
	home := shortHome(t)
	a := installPublished(t, home, "acme", "notes", "1.0.0", publishedRelease{})
	if err := SaveConfig(home, a.Name, Config{Version: ConfigVersion, Enabled: true, DisplayName: "Kept", Segments: 3}); err != nil {
		t.Fatal(err)
	}
	d := &dockerx.Fake{}
	if err := Uninstall(context.Background(), home, "acme/notes", d, false); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(d.Ops(), func(op string) bool { return strings.HasPrefix(op, "VolumeRemove:") }) {
		t.Errorf("an uninstall that keeps the data removed a volume: %v", d.Ops())
	}
	if _, err := LookupIn(home, "acme/notes"); err == nil {
		t.Error("the uninstalled agent is still one of the agents")
	}
	if c, err := LoadConfig(home, a.Name); err != nil || c.DisplayName != "Kept" || c.Segments != 3 {
		t.Fatalf("the kept configuration = %+v, %v", c, err)
	}
	again := installPublished(t, home, "acme", "notes", "1.0.0", publishedRelease{})
	if c, _ := LoadConfig(home, again.Name); c.DisplayName != "Kept" || c.Segments != 3 || !c.Enabled {
		t.Errorf("a reinstall did not take up the kept configuration: %+v", c)
	}

	owner, err := Acquire(home, again.Name, ClientCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background(), home, "acme/notes", d, true); err == nil {
		t.Error("a running agent was uninstalled")
	}
	owner.Release()
	if err := Uninstall(context.Background(), home, "acme/notes", d, true); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{SessionVolumeIn(home, a.Name), DataVolumeIn(home, a.Name), MemoryVolumeIn(home, a.Name)} {
		if !slices.Contains(d.Ops(), "VolumeRemove:"+v) {
			t.Errorf("--remove-data left %s: %v", v, d.Ops())
		}
	}
	if _, err := os.Stat(Dir(home, a.Name)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("--remove-data left the agent's folder: %v", err)
	}
	if err := Uninstall(context.Background(), home, "assistant", d, false); err == nil || !strings.Contains(err.Error(), "built into Agentry") {
		t.Errorf("uninstalling a built-in: %v", err)
	}
}

// TestAShippedPersonaIsTheAgentsDefault: a published agent goes by the
// persona its release ships, given at its first turn; the person's name and
// personality take its place, and a persona its publisher locked keeps its
// text whatever the person sets, though it may be renamed. The frame names a
// published agent by its own name when the person clears theirs.
func TestAShippedPersonaIsTheAgentsDefault(t *testing.T) {
	home := shortHome(t)
	open := installPublished(t, home, "acme", "ada", "1.0.0", publishedRelease{
		extra: "persona:\n  name: Ada\n", persona: "Brisk, warm and exact.\n"})
	if open.Persona == nil || open.Persona.Name != "Ada" || open.Persona.Text != "Brisk, warm and exact." || !open.Persona.Editable {
		t.Fatalf("persona = %+v", open.Persona)
	}
	var c Config
	if got := open.DisplayName(c); got != "Ada" {
		t.Errorf("display name %q, want the persona's", got)
	}
	if personaKey(open, c) == "" || !strings.Contains(personaFrame(open, c), `"Ada", and "Brisk, warm and exact."`) {
		t.Errorf("first frame %q", personaFrame(open, c))
	}
	name, mine := "Bea", "Calm."
	if _, err := applyChange(home, open, &c, Change{Name: &name, Personality: &mine}, false); err != nil {
		t.Fatal(err)
	}
	if f := personaFrame(open, c); !strings.Contains(f, `"Bea", and "Calm."`) {
		t.Errorf("the person's persona: %q", f)
	}

	locked := installPublished(t, home, "acme", "locked", "1.0.0", publishedRelease{
		extra: "persona:\n  name: Max\n  editable: false\n", persona: "Formal.\n"})
	var lc Config
	if _, err := applyChange(home, locked, &lc, Change{Personality: &mine}, false); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("replacing a locked persona: %v", err)
	}
	if _, err := applyChange(home, locked, &lc, Change{Name: &name}, false); err != nil {
		t.Errorf("renaming a locked persona: %v", err)
	}
	lc.Personality = "Smuggled in."
	if f := personaFrame(locked, lc); !strings.Contains(f, `"Formal."`) || strings.Contains(f, "Smuggled") {
		t.Errorf("a locked persona's frame %q", f)
	}

	plain := installPublished(t, home, "acme", "plain", "1.0.0", publishedRelease{})
	if f := personaFrame(plain, Config{}); !strings.Contains(f, `Go back to being "plain"`) {
		t.Errorf("a cleared published agent's frame %q", f)
	}
	if f := personaFrame(assistantAgent(t), Config{}); !strings.Contains(f, "Go back to being the Assistant") {
		t.Errorf("the Assistant's cleared frame %q", f)
	}
}

// TestShippedPacksAreFixedRosterEntries: each pack a published agent's
// release ships is a roster entry granted by the release, fixed and asking
// nothing, which no switch turns off; Apply leaves it to the segment, which
// reads it from the release; and a pack of the person's of the same name is
// refused.
func TestShippedPacksAreFixedRosterEntries(t *testing.T) {
	home := shortHome(t)
	a := installPublished(t, home, "acme", "guided", "1.0.0", publishedRelease{pack: true})
	if !slices.Equal(a.Packs, []string{"drafting-guide"}) {
		t.Fatalf("packs %v", a.Packs)
	}
	c := Config{Version: ConfigVersion, Enabled: true, Switches: map[string]bool{KnowledgeEntryID("drafting-guide"): false}}
	r := BuildRoster(a, c, nil)
	e, ok := r.Entry(KnowledgeEntryID("drafting-guide"))
	if !ok || e.GrantedBy != GrantedByRelease || !e.Fixed || !e.On || e.Kind != KindKnowledge {
		t.Fatalf("the shipped pack's entry = %+v (%t)", e, ok)
	}
	for _, p := range Unconsented(r, c) {
		if p.ID == e.ID {
			t.Error("the shipped pack asks to be agreed to again")
		}
	}
	if share, _ := r.Entry(EntryShare); share.GrantedBy != GrantedByRelease || !strings.Contains(share.Line, "checked for malware") && !strings.Contains(share.Line, "malware check is off") {
		t.Errorf("the share's entry = %+v", share)
	}
	// No opener: one asked for the shipped pack would fail Apply.
	g, _, done, err := Apply(context.Background(), home, a, c, r, nil, nil, nil)
	if err != nil {
		t.Fatalf("Apply asked the person's store for a shipped pack: %v", err)
	}
	done()
	if len(g.Knowledge) != 0 {
		t.Errorf("Apply gave the segment %d pack(s); the segment reads the shipped ones from its release", len(g.Knowledge))
	}
	pack := "drafting-guide"
	if _, err := applyChange(home, a, &c, Change{AddKnowledge: pack}, false); err == nil || !strings.Contains(err.Error(), "ships a knowledge pack named drafting-guide") {
		t.Errorf("adding a pack of the shipped pack's name: %v", err)
	}
}

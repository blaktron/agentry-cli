package engine

import (
	"archive/tar"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

var updateGolden = flag.Bool("update", false, "rewrite the engine's golden files")

const persistentManifest = `schemaVersion: "2"
name: keeper
namespace: agentry
modes: [persistent]
version: 1.0.0
summary: "persistent engine fixture"
publish:
  include: [agent.yaml, AGENT.md]
`

// persistentFixture compiles a persistent built-in's plan and the
// parameters of one segment of it.
func persistentFixture(t *testing.T, continueID string) (Params, *dockerx.Fake, *fakeHarness) {
	t.Helper()
	return persistentFixtureWith(t, continueID, nil)
}

// persistentFixtureWith is persistentFixture for a built-in granted its
// folders (compile.Inputs.AgentFiles); a memory among them gives the
// segment its memory volume and a seed.
func persistentFixtureWith(t *testing.T, continueID string, files *spec.AgentFiles) (Params, *dockerx.Fake, *fakeHarness) {
	t.Helper()
	m, err := manifest.Parse([]byte(persistentManifest))
	if err != nil {
		t.Fatal(err)
	}
	installDir := t.TempDir()
	ws := filepath.Join(installDir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENT.md"), []byte("Keep talking.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pol := policy.Default()
	pol.Images.Runner = "agentry-runner:test"
	pol.Sessions = policy.Sessions{IdleTimeoutSeconds: 60, MaxWallSeconds: 60, MaxUserTurns: 1}
	planDir := filepath.Join(installDir, "plan", "persistent")
	plan, err := compile.Compile(compile.Inputs{
		Manifest: m, Mode: manifest.ModePersistent, Digest: "sha256:feedface", Source: "builtin",
		WorkspaceDir: ws, Policy: pol, PolicySHA: "sha", RunnerImage: "agentry-runner:test", AgentFiles: files,
	}, planDir)
	if err != nil {
		t.Fatal(err)
	}
	ad, _ := adapter.Get(adapter.DefaultName)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key-9876")
	f := &dockerx.Fake{}
	happyFake(f)
	h := &fakeHarness{loadSession: true}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	p := Params{
		Plan: plan, PlanDir: planDir, WorkspaceSrc: ws, RunRoot: installDir, OutDir: t.TempDir(),
		Adapter: ad, Policy: pol, Attestation: receipt.Attestation{State: "builtin"},
		Stderr: os.Stderr, RunID: "5e9e0001",
		Persistent: &PersistentSegment{
			Agent: "keeper", Segment: 2, ContinueID: continueID,
			SessionVolume: "agentry-persistent-keeper-session", DataVolume: "agentry-persistent-keeper-data",
		},
	}
	if files != nil && files.Memory {
		p.Persistent.MemoryVolume = "agentry-persistent-keeper-memory"
		p.Persistent.MemorySeed = map[string][]byte{"SCHEMA.md": []byte("# How\n"), "pages/": nil, MemoryCarriedDir + "/log.md": []byte("carried\n")}
		p.Persistent.MemoryCarry, p.Persistent.MemoryCarryTo = "notes", "pages/notes"
	}
	return p, f, h
}

// TestPersistentPlanHasNoSessionCaps: a persistent plan's harness half is
// the interactive one, and none of the policy's session caps applies to it.
func TestPersistentPlanHasNoSessionCaps(t *testing.T) {
	p, _, _ := persistentFixture(t, "")
	if p.Plan.Mode != "persistent" || !p.Plan.Conversational() {
		t.Fatalf("mode = %q", p.Plan.Mode)
	}
	s := p.Plan.Harness.Session
	if s == nil || s.Transport != "acp-stdio" {
		t.Fatalf("session = %+v, want the interactive harness half", s)
	}
	if s.Budgets != (spec.SessionBudgets{}) {
		t.Errorf("session budgets = %+v, want none", s.Budgets)
	}
}

// TestPersistentPlanRunsOnlyAsASegment: a persistent plan never starts as
// an ordinary session, and a segment never starts from another plan.
func TestPersistentPlanRunsOnlyAsASegment(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	bare := p
	bare.Persistent = nil
	if _, err := StartSession(context.Background(), f, bare); err == nil || !strings.Contains(err.Error(), "persistent agent's segment") {
		t.Errorf("a persistent plan without its segment: err = %v", err)
	}
	ip, f2 := sessionFixture(t)
	ip.Persistent = p.Persistent
	if _, err := StartSession(context.Background(), f2, ip); err == nil || !strings.Contains(err.Error(), "persistent agent's segment") {
		t.Errorf("a segment of an interactive plan: err = %v", err)
	}
}

func runSegment(t *testing.T, p Params, f *dockerx.Fake) (*receipt.Envelope, []session.Event, PersistentSession) {
	t.Helper()
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ps, ok := s.(PersistentSession)
	if !ok {
		t.Fatal("a segment's session is not a PersistentSession")
	}
	get, done := collect(s)
	for range 3 {
		// No user-turn cap: the policy's would end the session at the second.
		if err := s.Send(context.Background(), "hello again"); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	env, err := s.End(context.Background(), false)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	<-done
	return env, get(), ps
}

// TestSweepKeepsPersistentVolumes: a segment makes the agent's three
// volumes (its session, its data and its memory) with the agent's label and
// no run label, mounts the session volume as the harness's store, seeds the
// new memory volume once, in a one-shot with no network that reads the data
// volume read-only, and neither its teardown nor the sweep of the segment's
// run once its process has died removes any of them. A second segment finds
// them and makes and seeds nothing.
func TestSweepKeepsPersistentVolumes(t *testing.T) {
	p, f, _ := persistentFixtureWith(t, "", &spec.AgentFiles{Memory: true})
	var seeds []dockerx.RunSpec
	var seeded []string
	f.Foreground["-memory-seed"] = &dockerx.FakeForeground{Handler: func(_ context.Context, r dockerx.RunSpec, stdin io.Reader, _, _ io.Writer) (int, error) {
		seeds = append(seeds, r)
		tr := tar.NewReader(stdin)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			seeded = append(seeded, h.Name)
		}
		return 0, nil
	}}
	env, _, _ := runSegment(t, p, f)
	if env.Status != "ok" {
		t.Fatalf("envelope %+v", env)
	}
	vols := map[string]map[string]string{}
	for _, v := range f.Volumes {
		vols[v.Name] = v.Labels
	}
	for _, name := range []string{p.Persistent.SessionVolume, p.Persistent.DataVolume, p.Persistent.MemoryVolume} {
		labels, ok := vols[name]
		if !ok {
			t.Errorf("volume %s is gone after the segment: %v", name, f.Ops())
			continue
		}
		if labels[PersistentLabel] != "keeper" || labels[runLabel] != "" {
			t.Errorf("volume %s labels = %v, want the agent's and no run's", name, labels)
		}
	}
	agent, ok := f.FindRun("-agent")
	if !ok {
		t.Fatal("no agent container")
	}
	mounted := false
	for _, m := range agent.Mounts {
		if m.Source == p.Persistent.SessionVolume && m.Target == spec.SessionMount {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("agent mounts = %+v, want the agent's session volume at %s", agent.Mounts, spec.SessionMount)
	}
	for _, op := range f.Ops() {
		if op == "VolumeCreate:"+sessionVolumeName(p.RunID) {
			t.Errorf("the segment made a run's session volume: %v", f.Ops())
		}
	}
	if len(seeds) != 1 {
		t.Fatalf("the memory volume was seeded %d times", len(seeds))
	}
	seed := seeds[0]
	wantMounts := []dockerx.Mount{{Source: p.Persistent.DataVolume, Target: "/data", ReadOnly: true}, {Source: p.Persistent.MemoryVolume, Target: "/memory"}}
	if seed.Network != "none" || !slices.Equal(seed.Mounts, wantMounts) || !slices.Equal(seed.CapAdd, []string{"DAC_READ_SEARCH"}) || !seed.Interactive {
		t.Errorf("the seed's one-shot: network %q mounts %+v caps %v interactive %v", seed.Network, seed.Mounts, seed.CapAdd, seed.Interactive)
	}
	if !slices.Equal(seed.Argv[len(seed.Argv)-3:], []string{"notes", "pages/notes", MemoryCarriedDir}) {
		t.Errorf("the seed's arguments: %v", seed.Argv)
	}
	if want := []string{MemoryCarriedDir + "/", MemoryCarriedDir + "/log.md", "SCHEMA.md", "pages/"}; !slices.Equal(seeded, want) {
		t.Errorf("the seed's archive holds %v, want %v", seeded, want)
	}
	if env.Persistent == nil || env.Persistent.Volumes.Memory != p.Persistent.MemoryVolume {
		t.Errorf("the envelope's volumes: %+v", env.Persistent)
	}

	// The segment's process died: its containers are swept by run.
	dead := deadOwner(t)
	f.Containers = []dockerx.Labeled{{Name: containerName(p.RunID, "agent"), Labels: withRun(dead, p.RunID)}}
	SweepAbandoned(context.Background(), f, nil)
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "VolumeRemove:agentry-persistent-") {
			t.Errorf("a persistent volume was removed: %s", op)
		}
	}
	if len(f.Volumes) != 3 {
		t.Errorf("volumes after the sweep = %+v", f.Volumes)
	}

	// The next segment finds all three and makes and seeds none.
	p2, _, _ := persistentFixtureWith(t, "", &spec.AgentFiles{Memory: true})
	p2.RunID = "5e9e0002"
	before := len(f.Ops())
	h := &fakeHarness{loadSession: true}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	runSegment(t, p2, f)
	for _, op := range f.Ops()[before:] {
		if strings.HasPrefix(op, "VolumeCreate:agentry-persistent-") {
			t.Errorf("the second segment made a volume that exists: %s", op)
		}
	}
	if len(seeds) != 1 {
		t.Errorf("the second segment seeded the memory again")
	}
}

// TestPersistentSegmentContinuesItsConversation: given the conversation the
// last segment was in, a harness that can load it is asked to, the replayed
// history is not this segment's events, and the envelope says it continued.
func TestPersistentSegmentContinuesItsConversation(t *testing.T) {
	p, f, h := persistentFixture(t, "s0")
	env, evs, ps := runSegment(t, p, f)
	h.mu.Lock()
	methods := strings.Join(h.methods, " ")
	h.mu.Unlock()
	if !strings.HasPrefix(methods, "initialize session/load session/prompt") {
		t.Errorf("harness saw %q, want session/load and no session/new", methods)
	}
	for _, ev := range evs {
		if strings.Contains(ev.Text, "an earlier") {
			t.Errorf("the replayed history reached the segment's events: %+v", ev)
		}
	}
	id, resumed, reason := ps.Conversation()
	if id != "s0" || !resumed || reason != "" {
		t.Errorf("conversation = %q %v %q", id, resumed, reason)
	}
	if env.Persistent == nil || !env.Persistent.Resumed || env.Harness.Mode != "persistent" {
		t.Errorf("envelope persistent = %+v, mode %q", env.Persistent, env.Harness.Mode)
	}
}

// TestPersistentSegmentStartsAnewWhenTheStoreCannotLoad: a harness that
// cannot load the stored conversation starts a new one, and the segment
// says why rather than failing.
func TestPersistentSegmentStartsAnewWhenTheStoreCannotLoad(t *testing.T) {
	p, f, h := persistentFixture(t, "s0")
	h.loadFails = true
	env, _, ps := runSegment(t, p, f)
	h.mu.Lock()
	methods := strings.Join(h.methods, " ")
	h.mu.Unlock()
	if !strings.HasPrefix(methods, "initialize session/load session/new session/prompt") {
		t.Errorf("harness saw %q", methods)
	}
	id, resumed, reason := ps.Conversation()
	if id != "s1" || resumed || !strings.Contains(reason, "could not load") {
		t.Errorf("conversation = %q %v %q", id, resumed, reason)
	}
	if env.Persistent == nil || env.Persistent.Resumed || !strings.Contains(env.Persistent.ResumeReason, "could not load") {
		t.Errorf("envelope persistent = %+v", env.Persistent)
	}
}

// TestPersistentSegmentReceipts holds a segment's receipts and its
// envelope's persistent block to a golden file: the files a segment
// writes, the mode the envelope names, and the block.
func TestPersistentSegmentReceipts(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	env, _, _ := runSegment(t, p, f)
	var files []string
	entries, err := os.ReadDir(filepath.Join(p.OutDir, "receipts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		// The docker filter's log is written where the filter runs (Linux).
		if e.Name() != dockerFilterLog {
			files = append(files, e.Name())
		}
	}
	slices.Sort(files)
	if _, err := os.Stat(filepath.Join(p.OutDir, "result.json")); err != nil {
		t.Errorf("no result.json: %v", err)
	}
	transcript, err := os.ReadFile(filepath.Join(p.OutDir, "receipts", "transcript.md"))
	if err != nil {
		t.Fatal(err)
	}
	if head := strings.SplitN(string(transcript), "\n", 4); len(head) < 3 || !strings.HasPrefix(head[2], "run 5e9e0001 · persistent · ") {
		t.Errorf("transcript head = %q, want the segment's run and its mode", head)
	}
	got, _ := json.MarshalIndent(map[string]any{
		"receipts":    files,
		"harnessMode": env.Harness.Mode,
		"persistent":  env.Persistent,
	}, "", "  ")
	got = append(got, '\n')
	golden := filepath.Join("testdata", "persistent-segment.golden.json")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("segment receipts:\n%s\nwant:\n%s", got, want)
	}
}

// TestACarriedContextReachesTheHarnessAndNotTheUserTurn: the harness is
// sent the carried context and the person's text; the user turn records the
// text alone, and the events receipt records what was carried.
func TestACarriedContextReachesTheHarnessAndNotTheUserTurn(t *testing.T) {
	p, f, h := persistentFixture(t, "")
	var prompts []string
	h.onPrompt = func(text string) { prompts = append(prompts, text) }
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	get, done := collect(s)
	if err := s.(PersistentSession).SendCarrying(context.Background(), "(earlier context)\n\n", "and now?"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.End(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-done
	if len(prompts) != 1 || prompts[0] != "(earlier context)\n\nand now?" {
		t.Errorf("the harness was sent %q", prompts)
	}
	carried := false
	for _, ev := range get() {
		if ev.Kind == session.KindUserTurn && ev.Text != "and now?" {
			t.Errorf("user turn = %q, want the person's text alone", ev.Text)
		}
		if ev.Kind == session.KindOther && strings.Contains(ev.Text, "earlier context") {
			carried = true
		}
	}
	if !carried {
		t.Error("the carried context is in no event")
	}
	md, _ := os.ReadFile(filepath.Join(p.OutDir, "receipts", "transcript.md"))
	if !strings.Contains(string(md), "## User\n\nand now?\n") {
		t.Errorf("transcript.md does not show the person's text alone as theirs:\n%s", md)
	}
}

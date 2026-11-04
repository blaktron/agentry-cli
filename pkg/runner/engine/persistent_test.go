package engine

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
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
// folders (compile.Inputs.AgentFiles); a memory among them, which its
// manifest then declares, gives the segment its memory volume and a seed.
func persistentFixtureWith(t *testing.T, continueID string, files *spec.AgentFiles) (Params, *dockerx.Fake, *fakeHarness) {
	t.Helper()
	text := persistentManifest
	if files != nil && files.Memory {
		text = strings.Replace(text, "modes: [persistent]\n", "modes: [persistent]\ncapabilities:\n  persistent:\n    memory: true\n", 1)
	}
	m, err := manifest.Parse([]byte(text))
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
		p.Persistent.Memory = &PersistentMemory{Volume: "agentry-persistent-keeper-memory",
			Seed: map[string][]byte{"SCHEMA.md": []byte("# How\n"), "index.md": []byte("# Index\n"), "pages/": nil, "pages/a.md": []byte("a\n"), "pages/b.md": []byte("b\n"),
				MemoryCarriedDir + "/log.md": []byte("carried\n")},
			Carry: "notes", CarryTo: "pages/notes"}
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
	// A segment names its agent and both of its volumes.
	for name, edit := range map[string]func(*PersistentSegment){
		"no agent":          func(ps *PersistentSegment) { ps.Agent = "" },
		"no session volume": func(ps *PersistentSegment) { ps.SessionVolume = "" },
		"no data volume":    func(ps *PersistentSegment) { ps.DataVolume = "" },
	} {
		seg := *p.Persistent
		edit(&seg)
		half := p
		half.Persistent = &seg
		if _, err := StartSession(context.Background(), f, half); err == nil || !strings.Contains(err.Error(), "needs its agent's name and volumes") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("a refused segment touched the engine: %v", f.Ops())
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
	var headers []*tar.Header
	f.Foreground["-memory-seed"] = &dockerx.FakeForeground{Handler: func(_ context.Context, r dockerx.RunSpec, stdin io.Reader, _, _ io.Writer) (int, error) {
		seeds = append(seeds, r)
		tr := tar.NewReader(stdin)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			seeded = append(seeded, h.Name)
			headers = append(headers, h)
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
	for _, name := range []string{p.Persistent.SessionVolume, p.Persistent.DataVolume, p.Persistent.Memory.Volume} {
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
	wantMounts := []dockerx.Mount{{Source: p.Persistent.DataVolume, Target: "/data", ReadOnly: true}, {Source: p.Persistent.Memory.Volume, Target: "/memory"}}
	if seed.Network != "none" || !slices.Equal(seed.Mounts, wantMounts) || !slices.Equal(seed.CapAdd, []string{"DAC_READ_SEARCH"}) || !seed.Interactive {
		t.Errorf("the seed's one-shot: network %q mounts %+v caps %v interactive %v", seed.Network, seed.Mounts, seed.CapAdd, seed.Interactive)
	}
	if !slices.Equal(seed.Argv[len(seed.Argv)-3:], []string{"notes", "pages/notes", MemoryCarriedDir}) {
		t.Errorf("the seed's arguments: %v", seed.Argv)
	}
	if want := []string{MemoryCarriedDir + "/", MemoryCarriedDir + "/log.md", "SCHEMA.md", "index.md", "pages/", "pages/a.md", "pages/b.md"}; !slices.Equal(seeded, want) {
		t.Errorf("the seed's archive holds %v, want %v", seeded, want)
	}
	// Everything in the archive is root's, folders 0755 and files 0644.
	for _, h := range headers {
		wantMode := int64(0o644)
		if h.Typeflag == tar.TypeDir {
			wantMode = 0o755
		}
		if h.Uid != 0 || h.Gid != 0 || h.Mode != wantMode {
			t.Errorf("%s is %d:%d mode %o, want root and %o", h.Name, h.Uid, h.Gid, h.Mode, wantMode)
		}
	}
	// The one-shot is the runner image's shell, as root, bounded as every
	// run container is, labelled with the run, and removed when it is done.
	lim := p.limits()
	if seed.Entrypoint != "sh" || seed.User != "0:0" || seed.Image != p.Plan.Proxy.Image || seed.Image == "" || !seed.AutoRemove || seed.Labels[runLabel] != p.RunID ||
		lim.Memory == "" || seed.Memory != lim.Memory || seed.CPUs != lim.CPUs || seed.PidsLimit != lim.Pids || !seed.NoNewPrivileges || !slices.Equal(seed.CapDrop, []string{"ALL"}) {
		t.Errorf("the seed's one-shot: %+v", seed)
	}
	if !slices.Contains(f.Ops(), "RemoveForce:"+seed.Name) {
		t.Errorf("the seed's one-shot was not removed: %v", f.Ops())
	}
	if env.Persistent == nil || env.Persistent.Volumes.Memory != p.Persistent.Memory.Volume ||
		env.Persistent.Volumes.Session != p.Persistent.SessionVolume || env.Persistent.Volumes.Data != p.Persistent.DataVolume {
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

	// The next segment finds all three, its memory seeded, and makes and
	// seeds none.
	p2, _, _ := persistentFixtureWith(t, "", &spec.AgentFiles{Memory: true})
	p2.Persistent.Memory.Seeded = true
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

	// A memory volume no start recorded seeding (one stopped between
	// making it and seeding it) is seeded again, and neither made again
	// nor removed when that seed fails.
	p3, _, _ := persistentFixtureWith(t, "", &spec.AgentFiles{Memory: true})
	p3.RunID = "5e9e0003"
	f.Foreground["-memory-seed"] = &dockerx.FakeForeground{ExitCode: 1, Stderr: "no space left"}
	before = len(f.Ops())
	if _, err := StartSession(context.Background(), f, p3); err == nil || !strings.Contains(err.Error(), "seed the memory volume") || !strings.Contains(err.Error(), "the seed exited 1: no space left") {
		t.Fatalf("a failed seed of an existing memory: %v", err)
	}
	// A segment that failed before its harness started says so.
	var failed receipt.Envelope
	if b, err := os.ReadFile(filepath.Join(p3.OutDir, "result.json")); err != nil || json.Unmarshal(b, &failed) != nil || failed.Persistent == nil ||
		failed.Persistent.Resumed || failed.Persistent.ResumeReason != notStartedReason || failed.Persistent.Segment != p3.Persistent.Segment {
		t.Errorf("the failed segment's envelope: %+v (%v)", failed.Persistent, err)
	}
	for _, op := range f.Ops()[before:] {
		if op == "VolumeCreate:"+p3.Persistent.Memory.Volume || op == "VolumeRemove:"+p3.Persistent.Memory.Volume {
			t.Errorf("the existing memory volume was made or removed: %s", op)
		}
	}
	if !slices.ContainsFunc(f.Ops()[before:], func(op string) bool { return strings.Contains(op, "-memory-seed") }) {
		t.Errorf("the unrecorded memory was not seeded: %v", f.Ops()[before:])
	}

	// A memory volume made by this start and not seeded is removed, so the
	// next start makes and seeds it again.
	p4, _, _ := persistentFixtureWith(t, "", &spec.AgentFiles{Memory: true})
	p4.RunID = "5e9e0004"
	p4.Persistent.Memory.Volume = "agentry-persistent-keeper-fresh-memory"
	before = len(f.Ops())
	if _, err := StartSession(context.Background(), f, p4); err == nil || !strings.Contains(err.Error(), "seed the memory volume") {
		t.Fatalf("a failed seed of a new memory: %v", err)
	}
	if ops := f.Ops()[before:]; !slices.Contains(ops, "VolumeCreate:"+p4.Persistent.Memory.Volume) || !slices.Contains(ops, "VolumeRemove:"+p4.Persistent.Memory.Volume) {
		t.Errorf("the new memory volume was not made and removed: %v", ops)
	}

	// Under the plan's runtime, the seed's one-shot runs under it too.
	p6, f6, _ := persistentFixtureWith(t, "", &spec.AgentFiles{Memory: true})
	p6.Plan.Docker = &spec.DockerSpec{Runtime: "runsc"}
	if err := ensurePersistentVolumes(context.Background(), f6, p6); err != nil {
		t.Fatal(err)
	}
	if r, ok := f6.FindRun("-memory-seed"); !ok || r.Runtime != "runsc" {
		t.Errorf("the seed's one-shot under the runtime: %+v (found %v)", r, ok)
	}

	// A seed that names a path outside the memory is refused before any
	// container runs.
	for _, bad := range []string{"../escape", "/absolute", "a/../b", "a//b", ".", "./"} {
		p5, _, _ := persistentFixtureWith(t, "", &spec.AgentFiles{Memory: true})
		p5.RunID = "5e9e0005"
		p5.Persistent.Memory.Seed = map[string][]byte{bad: []byte("x")}
		before = len(f.Ops())
		if _, err := StartSession(context.Background(), f, p5); err == nil || !strings.Contains(err.Error(), "that is not a clean relative one") {
			t.Errorf("the seed path %q: %v", bad, err)
		}
		if slices.ContainsFunc(f.Ops()[before:], func(op string) bool { return strings.Contains(op, "-memory-seed") }) {
			t.Errorf("a seed with the path %q ran: %v", bad, f.Ops()[before:])
		}
	}
}

// TestPersistentSegmentContinuesItsConversation: given the conversation the
// last segment was in, a harness that can load it is asked to, the replayed
// history is not this segment's events, and the envelope says it continued.
func TestPersistentSegmentContinuesItsConversation(t *testing.T) {
	p, f, h := persistentFixture(t, "s0")
	p.Persistent.Roster = []receipt.RosterEntry{{ID: "clock", Kind: "server", Name: "Clock", GrantedBy: "the manifest", On: true, Line: "tells the time"}}
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
	if env.Persistent != nil && !slices.Equal(env.Persistent.Roster, p.Persistent.Roster) {
		t.Errorf("the envelope's roster = %+v, want %+v", env.Persistent.Roster, p.Persistent.Roster)
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

// TestASegmentStartsANewConversationInItsRunningHarness: NewConversation asks
// the running harness for a new session between turns, with no second
// harness started; the segment names the conversation it is now in and why,
// and the cost the conversation it left spent is kept under the new one's.
func TestASegmentStartsANewConversationInItsRunningHarness(t *testing.T) {
	p, f, h := persistentFixture(t, "")
	p.Policy.Sessions.MaxUserTurns = 0
	h.usage = map[string]any{"used": 2000, "size": 200000, "cost": map[string]any{"amount": 0.25, "currency": "USD"}}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	get, done := collect(s)
	if err := s.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	r, ok := s.(ConversationRenewer)
	if !ok {
		t.Fatal("a segment's session cannot start a new conversation")
	}
	id, err := r.NewConversation(context.Background(), "a compaction")
	if err != nil || id == "" {
		t.Fatalf("NewConversation = %q, %v", id, err)
	}
	if got, resumed, reason := s.(PersistentSession).Conversation(); got != id || resumed || reason != "a compaction" {
		t.Errorf("Conversation = %q %v %q", got, resumed, reason)
	}
	if err := s.Send(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	env, err := s.End(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	news := 0
	h.mu.Lock()
	for _, m := range h.methods {
		if m == "session/new" {
			news++
		}
	}
	h.mu.Unlock()
	if news != 2 {
		t.Errorf("session/new was asked %d times, want 2", news)
	}
	agents := 0
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "RunForeground:") && strings.HasSuffix(op, "-agent") {
			agents++
		}
	}
	if agents != 1 {
		t.Errorf("%d agent containers started, want the one: %v", agents, f.Ops())
	}
	if nc := env.Persistent.NewConversations; len(nc) != 1 || nc[0].ID != id || nc[0].Reason != "a compaction" || nc[0].At.IsZero() {
		t.Errorf("the envelope's new conversations = %+v", nc)
	}
	if env.Usage.CostUSD != 0.5 {
		t.Errorf("the segment's cost = %v, want both conversations' 0.5", env.Usage.CostUSD)
	}
	said := false
	for _, ev := range get() {
		if ev.Kind == session.KindOther && ev.Text == "a new conversation in the running harness: a compaction" {
			said = true
		}
	}
	if !said {
		t.Errorf("no event says the segment is in a new conversation: %+v", get())
	}
	if _, err := r.NewConversation(context.Background(), "late"); !errors.Is(err, session.ErrEnded) {
		t.Errorf("NewConversation after the end = %v", err)
	}
}

// TestASegmentsConversationNotesAreMasked: the reason a segment gives for a
// new conversation, and the one the supervisor replaces its resume reason
// with, are masked in the segment's events, its envelope and what the
// supervisor reads back.
func TestASegmentsConversationNotesAreMasked(t *testing.T) {
	const secret = "sk-test-key-9876"
	p, f, _ := persistentFixture(t, "s0")
	p.Policy.Sessions.MaxUserTurns = 0
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	get, done := collect(s)
	if err := s.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	id, err := s.(ConversationRenewer).NewConversation(context.Background(), "compacting "+secret)
	if err != nil {
		t.Fatal(err)
	}
	// A new conversation is not the one the segment continued.
	if got, resumed, reason := s.(PersistentSession).Conversation(); got != id || got == "s0" || resumed || reason != "compacting [redacted]" {
		t.Errorf("Conversation = %q %v %q", got, resumed, reason)
	}
	s.(PersistentSession).SetResumeReason("the supervisor went on with " + secret)
	if _, resumed, reason := s.(PersistentSession).Conversation(); resumed || reason != "the supervisor went on with [redacted]" {
		t.Errorf("after SetResumeReason, Conversation = %v %q", resumed, reason)
	}
	env, err := s.End(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	<-done
	if env.Persistent == nil || env.Persistent.ResumeReason != "the supervisor went on with [redacted]" ||
		len(env.Persistent.NewConversations) != 1 || env.Persistent.NewConversations[0].Reason != "compacting [redacted]" {
		t.Errorf("the envelope's persistent block: %+v", env.Persistent)
	}
	noted := false
	for _, ev := range get() {
		if strings.Contains(ev.Text, secret) {
			t.Errorf("an event holds the secret: %+v", ev)
		}
		noted = noted || ev.Text == "a new conversation in the running harness: compacting [redacted]"
	}
	if !noted {
		t.Errorf("no event says why the segment is in a new conversation: %+v", get())
	}
}

// TestASegmentWhoseHarnessCannotCompactSaysSo: docker-agent's driver has no
// compaction on request (its harness clears old results itself), so the
// segment refuses with ErrNoCompaction and its conversation is unchanged.
func TestASegmentWhoseHarnessCannotCompactSaysSo(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, done := collect(s)
	if err := s.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.(PersistentSession).Conversation()
	c, ok := s.(ConversationCompactor)
	if !ok {
		t.Fatal("a segment's session has no compaction call")
	}
	if c.CanCompact() {
		t.Error("a segment whose harness cannot compact says it can")
	}
	if err := c.CompactConversation(context.Background()); !errors.Is(err, ErrNoCompaction) {
		t.Fatalf("CompactConversation = %v, want ErrNoCompaction", err)
	}
	if after, _, _ := s.(PersistentSession).Conversation(); after != before {
		t.Errorf("conversation %q became %q", before, after)
	}
	if _, err := s.End(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := c.CompactConversation(context.Background()); !errors.Is(err, session.ErrEnded) {
		t.Errorf("CompactConversation after the end = %v, want session.ErrEnded", err)
	}

	// A compaction between turns is asked of the running harness, moves the turn count on and is
	// noted in the events; one the harness fails leaves the count and the
	// events as they were.
	t.Run("a harness that compacts is asked to, between turns", func(t *testing.T) {
		for name, failure := range map[string]error{"it compacts": nil, "it fails": errors.New("nothing to compact")} {
			t.Run(name, func(t *testing.T) {
				p, f, _ := persistentFixture(t, "")
				p.Policy.Sessions.MaxUserTurns = 0
				calls := 0
				p.Adapter = compactingAdapter{Adapter: p.Adapter, err: failure, calls: &calls}
				s, err := StartSession(context.Background(), f, p)
				if err != nil {
					t.Fatalf("start: %v", err)
				}
				get, done := collect(s)
				if err := s.Send(context.Background(), "one"); err != nil {
					t.Fatal(err)
				}
				c := s.(ConversationCompactor)
				if !c.CanCompact() {
					t.Error("a segment whose harness compacts says it cannot")
				}
				before, _, _ := s.(PersistentSession).Conversation()
				turns := func() string {
					b, err := os.ReadFile(filepath.Join(s.(*liveSession).rd.turns, spec.TurnsFile))
					if err != nil {
						t.Fatal(err)
					}
					return strings.TrimSpace(string(b))
				}
				err = c.CompactConversation(context.Background())
				if !errors.Is(err, failure) || calls != 1 {
					t.Fatalf("CompactConversation = %v after %d requests, want %v after one", err, calls, failure)
				}
				wantTurns := "2"
				if failure != nil {
					wantTurns = "1"
				}
				if got := turns(); got != wantTurns {
					t.Errorf("the turn count after the compaction = %q, want %q", got, wantTurns)
				}
				if after, _, _ := s.(PersistentSession).Conversation(); after != before {
					t.Errorf("conversation %q became %q", before, after)
				}
				if _, err := s.End(context.Background(), false); err != nil {
					t.Fatal(err)
				}
				<-done
				noted := 0
				for _, ev := range get() {
					if ev.Kind == session.KindOther && ev.Text == "the running harness compacted the conversation" {
						noted++
					}
				}
				if want := map[bool]int{true: 1, false: 0}[failure == nil]; noted != want {
					t.Errorf("the compaction was noted %d times, want %d", noted, want)
				}
			})
		}
	})
}

// compactingAdapter is the fixture's adapter whose driver compacts its
// conversation on request, failing with err when that is set, and counts
// the requests.
type compactingAdapter struct {
	adapter.Adapter
	err   error
	calls *int
}

func (a compactingAdapter) Session(ctx context.Context, rw io.ReadWriteCloser, o adapter.SessionOptions) (adapter.SessionDriver, error) {
	d, err := a.Adapter.Session(ctx, rw, o)
	if err != nil {
		return nil, err
	}
	return &compactingDriver{SessionDriver: d, err: a.err, calls: a.calls}, nil
}

type compactingDriver struct {
	adapter.SessionDriver
	err   error
	calls *int
}

func (d *compactingDriver) CompactConversation(context.Context) error {
	*d.calls++
	return d.err
}

// TestTheTurnCountMovesAtEachTurnAndNewConversation: the run's turn count,
// which the tools server's re-read stub reads, starts at 0 and moves on at
// every turn's start and at a new conversation.
func TestTheTurnCountMovesAtEachTurnAndNewConversation(t *testing.T) {
	p, f, _ := persistentFixture(t, "")
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, done := collect(s)
	ls := s.(*liveSession)
	count := func() string {
		b, err := os.ReadFile(filepath.Join(ls.rd.turns, spec.TurnsFile))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	if got := count(); got != "0" {
		t.Fatalf("at the start the count = %q, want 0", got)
	}
	if err := s.Send(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != "1" {
		t.Fatalf("after a turn the count = %q, want 1", got)
	}
	if _, err := s.(ConversationRenewer).NewConversation(context.Background(), "a compaction"); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != "2" {
		t.Fatalf("after a new conversation the count = %q, want 2", got)
	}
	if _, err := s.End(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	<-done
}

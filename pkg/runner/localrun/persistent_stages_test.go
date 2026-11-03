package localrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/knowledge/knowledgetest"
	persistentassistant "github.com/blaktron/agentry-cli/pkg/persistent/assistant"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/harnessbuild"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
	"github.com/blaktron/agentry-cli/pkg/runner/osvdb"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/pkg/trust"
	"github.com/blaktron/agentry-cli/runnerimage"
	"github.com/blaktron/agentry-cli/runnerimage/probe"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
	"github.com/blaktron/agentry/pkg/manifest"
)

// TestASegmentStartReportsEachStageOnceInOrder: a persistent agent's start
// reports each of its steps once, in order, from the plan's compile to the
// harness; a server the store does not hold is named while it installs, and
// one it holds is not. A start with no hook reports to no one and starts the
// same.
func TestASegmentStartReportsEachStageOnceInOrder(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key-9876")
	pol := wordServerRegistry(t)
	home, store := t.TempDir(), t.TempDir()
	builds := 0
	fake := &dockerx.Fake{
		ExistingImages: map[string]bool{
			pol.Images.PythonRuntime: true, pol.Images.Gateway: true, pol.Images.HarnessDockerAgent: true,
			pol.RunnerImageTag(runnerimage.ContentVersion()): true,
		},
		Foreground: map[string]*dockerx.FakeForeground{
			"agentry-mcpinstall-pypi-some-word-server": wordServerBuild(t, &builds),
			"-probe": {Handler: func(_ context.Context, r dockerx.RunSpec, _ io.Reader, stdout, _ io.Writer) (int, error) {
				if slices.Contains(r.Argv, "--list-tools") {
					b, _ := json.Marshal(probe.Listing{Tools: []string{}})
					_, _ = stdout.Write(append(b, '\n'))
				}
				return 0, nil
			}},
		},
	}
	segment := func(n int, onStage func(engine.Stage)) Segment {
		return Segment{Builtin: Assistant(), Home: home, Policy: pol, PolicySHA: "s", Build: "test",
			OutDir: t.TempDir(), RunID: fmt.Sprintf("5e9e%04d", n), OnStage: onStage,
			Persistent: engine.PersistentSegment{Agent: "assistant", Segment: n,
				SessionVolume: "agentry-persistent-assistant-session", DataVolume: "agentry-persistent-assistant-data",
				Memory: &engine.PersistentMemory{Volume: "agentry-persistent-assistant-memory"}},
			Grants: Grants{
				Servers:    []manifest.MCPServer{{PyPI: "some-word-server", Version: "1.1.11", Tools: []string{"create_document"}}},
				ServersDir: store,
			}}
	}
	// The fake harness exits at once, so each start ends at the harness's
	// handshake, after every step has been reached.
	start := func(seg Segment) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sess, err := seg.Start(ctx, fake)
		if err == nil {
			_, _ = sess.End(context.Background(), false)
			t.Fatal("the start with a harness that exits at once did not fail")
		}
		if !strings.Contains(err.Error(), "interactive session") {
			t.Fatalf("the start failed before the harness: %v", err)
		}
	}

	var got []string
	record := func(st engine.Stage) { got = append(got, st.Name+"/"+st.Subject+": "+st.Words()) }
	start(segment(1, record))
	want := []string{
		"compile/: Preparing",
		"images/: Getting images ready",
		"mcp-servers/some-word-server: Installing some-word-server",
		"prepare/: Preparing",
		"infra/: Starting the gateway",
		"agent/: Starting the agent",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if builds != 1 {
		t.Fatalf("the server was built %d times, want once", builds)
	}

	// The next start finds the server in the store and does not say it is
	// installing it.
	got = nil
	start(segment(2, record))
	if slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "mcp-servers/") }) {
		t.Errorf("a start with the server stored reported installing it: %v", got)
	}

	// No hook: the same start, reported to no one.
	start(segment(3, nil))
	if builds != 1 {
		t.Errorf("the server was built %d times, want once", builds)
	}

	// A start whose model is reached through a subscription's sign-in reports
	// that step too, once, between the plan and the images; a start on a key
	// reports none.
	signPol := policy.Default()
	signPol.Model = policy.Model{Provider: policy.ProviderAnthropic, Name: "claude-sonnet-4-6", APIKeyKind: policy.KeyKindClaudeSignIn,
		ProxyAllow: []string{"api.anthropic.com:443", "platform.claude.com:443"}}
	harness, _ := signPol.Images.HarnessImageFor("claude-image")
	signFake := &dockerx.Fake{
		ExistingImages: map[string]bool{signPol.Images.Gateway: true, harness.Ref: true, signPol.RunnerImageTag(runnerimage.ContentVersion()): true},
		Labels:         map[string]map[string]string{harness.Ref: {harnessbuild.LabelArtifact: harness.Artifact, harnessbuild.LabelRecipe: harnessbuild.ClaudeRecipe}},
		Foreground:     fake.Foreground,
	}
	signHome := t.TempDir()
	if _, err := modelsignin.Open(signHome, nil).Put(modelsignin.Claude, modelsignin.Record{ConfigDir: "agentry-claude-signin"}, modelsignin.Grant{}); err != nil {
		t.Fatal(err)
	}
	var signed []string
	signSeg := segment(4, func(st engine.Stage) { signed = append(signed, st.Name) })
	signSeg.Home, signSeg.Policy, signSeg.Grants = signHome, signPol, Grants{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if sess, err := signSeg.Start(ctx, signFake); err == nil {
		_, _ = sess.End(context.Background(), false)
		t.Fatal("the start with a harness that exits at once did not fail")
	} else if !strings.Contains(err.Error(), "interactive session") {
		t.Fatalf("the sign-in start failed before the harness: %v", err)
	}
	if strings.Join(signed, ",") != "compile,sign-in,images,prepare,infra,agent" {
		t.Errorf("stages of a start on a sign-in: %v", signed)
	}
	if agent, ok := signFake.FindRun("-agent"); !ok || !slices.ContainsFunc(agent.Mounts, func(m dockerx.Mount) bool { return m.Source == "agentry-claude-signin" }) {
		t.Errorf("the agent container does not mount the sign-in's volume: %+v", agent.Mounts)
	}
}

// startSeen is what a segment's start handed the engine, read where the
// gateway starts: the catalog and the secrets it was given, the packs staged
// for it, and what the agent's container was started with.
type startSeen struct {
	catalog, secrets string
	toolspec         string
	bearers          map[string]string
	runPlan          string
	packs            []string
	ops              []string
	agent            dockerx.RunSpec
	meta, result     map[string]any
	home             string
	err              error
}

// startAndSee starts seg on a fake daemon whose harness exits at once, the
// start ending at the harness's handshake, after the gateway has been given
// everything it is to serve. toolchain is the Coding agent's toolchain record.
func startAndSee(t *testing.T, seg Segment, toolchain bool) startSeen {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key-9876")
	pol := seg.Policy
	runner := pol.RunnerImageTag(runnerimage.ContentVersion())
	f := &dockerx.Fake{
		ExistingImages: map[string]bool{pol.Images.Gateway: true, pol.Images.HarnessDockerAgent: true, runner: true},
		Foreground: map[string]*dockerx.FakeForeground{
			"-probe": {Handler: func(_ context.Context, r dockerx.RunSpec, _ io.Reader, stdout, _ io.Writer) (int, error) {
				if slices.Contains(r.Argv, "--list-tools") {
					b, _ := json.Marshal(probe.Listing{Tools: []string{}})
					_, _ = stdout.Write(append(b, '\n'))
				}
				return 0, nil
			}},
		},
	}
	seen := startSeen{home: seg.Home}
	f.OnDetached = func(r dockerx.RunSpec) {
		for _, m := range r.Mounts {
			if m.Target != "/root/.docker/mcp/catalogs" {
				continue
			}
			cfg := filepath.Dir(m.Source)
			catalog, _ := os.ReadFile(filepath.Join(cfg, "gateway", "catalog.yaml"))
			secrets, _ := os.ReadFile(filepath.Join(cfg, "gateway", "secrets.env"))
			seen.catalog, seen.secrets = string(catalog), string(secrets)
			toolspec, _ := os.ReadFile(filepath.Join(cfg, "gateway", "toolspec.json"))
			seen.toolspec = string(toolspec)
			conns, _ := os.ReadDir(filepath.Join(cfg, "connections"))
			seen.bearers = map[string]string{}
			for _, c := range conns {
				bearer, _ := os.ReadFile(filepath.Join(cfg, "connections", c.Name(), "bearer"))
				seen.bearers[c.Name()] = string(bearer)
			}
			entries, _ := os.ReadDir(filepath.Join(filepath.Dir(cfg), "knowledge"))
			for _, e := range entries {
				seen.packs = append(seen.packs, e.Name())
			}
		}
	}
	f.OnForeground = func(r dockerx.RunSpec) {
		if strings.HasSuffix(r.Name, "-agent") {
			seen.agent = r
		}
	}
	if toolchain {
		sp, err := seg.Plan(context.Background(), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		f.ExistingImages[sp.Plan.ExecImage.Tag] = true
		f.ImageFiles = map[string]string{sp.Plan.ExecImage.Tag + ":/etc/agentry/coder/record": "git 2.45.0\n"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := seg.Start(ctx, f)
	if err == nil {
		_, _ = sess.End(context.Background(), false)
	}
	seen.err, seen.ops = err, f.Ops()
	plan, _ := os.ReadFile(filepath.Join(seg.OutDir, "receipts", "run-plan.json"))
	seen.runPlan = string(plan)
	for name, into := range map[string]*map[string]any{"result.json": &seen.result, filepath.Join("receipts", "meta.json"): &seen.meta} {
		if b, rerr := os.ReadFile(filepath.Join(seg.OutDir, name)); rerr == nil {
			_ = json.Unmarshal(b, into)
		}
	}
	return seen
}

// TestASegmentStartGivesTheEngineWhatItsRosterTurnsOn: each folder, socket,
// secret and pack the roster turns on reaches the segment's gateway and its
// agent, and nothing reaches it that the roster did not turn on; a start sets
// the segment's attestation and holds it to no session cap.
func TestASegmentStartGivesTheEngineWhatItsRosterTurnsOn(t *testing.T) {
	segment := func(b Builtin, g Grants) Segment {
		seg := Segment{Builtin: b, Home: t.TempDir(), Policy: policy.Default(), PolicySHA: "s", Build: "test", OutDir: t.TempDir(), RunID: "5e9e0007", Grants: g,
			Persistent: engine.PersistentSegment{Agent: "agent", Segment: 1, SessionVolume: "agentry-persistent-agent-session", DataVolume: "agentry-persistent-agent-data"}}
		if b.KeepsMemory() {
			seg.Persistent.Memory = &engine.PersistentMemory{Volume: "agentry-persistent-agent-memory"}
		}
		return seg
	}
	refused := func(seen startSeen) {
		t.Helper()
		if seen.err == nil || !strings.Contains(seen.err.Error(), "interactive session") {
			t.Fatalf("the start failed before the harness: %v", seen.err)
		}
	}

	share, skills := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(skills, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "notes", "SKILL.md"), []byte("---\nname: notes\ndescription: Use it.\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "app.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	assistant := segment(Assistant(), Grants{
		Share: share, AppLink: sock, SkillsDir: skills, Skills: []spec.Skill{{Name: "notes", Description: "Use it."}},
		ImageServers:  []ImageGrant{{Key: "gog", Volume: "vol-gog"}},
		ServerSecrets: map[string]string{"GOG_KEYRING_PASSWORD": "keyring-pw"},
		Secrets:       map[string]string{"NOTE_KEY": "note-value"},
		CalendarZone:  "Europe/Paris", CalendarFeeds: []CalendarFeed{{Name: "Work", Host: "calendar.example.com", URL: "https://calendar.example.com/work.ics"}},
		Knowledge: []engine.AttachedPack{engine.AttachPack(knowledgetest.Make(t, "runbooks", nil))},
	})
	seen := startAndSee(t, assistant, false)
	refused(seen)
	for _, want := range []string{share + ":/agent/share", skills + ":/agent/skills:ro", sock + ":/run/agentry/app.sock"} {
		if !strings.Contains(seen.catalog, want) {
			t.Errorf("the gateway's catalog lacks %q:\n%s", want, seen.catalog)
		}
	}
	if !slices.Contains(seen.ops, "VolumeCreate:vol-gog") {
		t.Errorf("the image server's volume was not made: %v", seen.ops)
	}
	// The secrets are the ones a server of the roster declares: an image
	// server's from the segment's own, a calendar feed's address for the tools
	// server; a stored secret no server declares reaches nothing.
	for _, want := range []string{"mcp-image-gog.gog_keyring_password=keyring-pw", "agentry-tools.agentry_calendar_feed_1=https://calendar.example.com/work.ics"} {
		if !strings.Contains(seen.secrets, want) {
			t.Errorf("the gateway's secrets lack %q:\n%s", want, seen.secrets)
		}
	}
	if strings.Contains(seen.secrets, "note-value") {
		t.Errorf("a stored secret no server declares reached the gateway:\n%s", seen.secrets)
	}
	if !slices.Equal(seen.packs, []string{"runbooks"}) {
		t.Errorf("packs staged for the segment: %v", seen.packs)
	}
	if got, _ := seen.result["attestation"].(map[string]any); got["state"] != "builtin" || got["build"] != "test" || got["verified"] != false {
		t.Errorf("the segment's attestation %v", seen.result["attestation"])
	}
	if everything, perr := spec.ParsePlan([]byte(seen.runPlan)); perr != nil || everything.Capabilities.AgentFiles == nil ||
		len(everything.Capabilities.AgentFiles.Skills) != 1 || everything.Capabilities.AgentFiles.Skills[0].Name != "notes" {
		t.Errorf("the plan's folders: %v %+v", perr, everything.Capabilities.AgentFiles)
	}

	// Fastmail's remote server is given the stored token as its bearer, and no
	// value when the roster has the secret off.
	fastmail := []manifest.MCPServer{persistentassistant.Fastmail().Server}
	seen = startAndSee(t, segment(Assistant(), Grants{Servers: fastmail, Secrets: map[string]string{persistentassistant.FastmailToken: "fm-token"}}), false)
	refused(seen)
	if len(seen.bearers) != 1 {
		t.Fatalf("connections %v", seen.bearers)
	}
	for _, bearer := range seen.bearers {
		if bearer != "fm-token" {
			t.Errorf("the stored token reached the server as %q", bearer)
		}
	}
	seen = startAndSee(t, segment(Assistant(), Grants{Servers: fastmail, WithheldSecrets: []string{persistentassistant.FastmailToken}}), false)
	refused(seen)
	for _, bearer := range seen.bearers {
		if bearer != "" {
			t.Errorf("a secret the roster has off reached the server as %q", bearer)
		}
	}

	// An image server's secret needs no stored secret beside it.
	seen = startAndSee(t, segment(Assistant(), Grants{ImageServers: []ImageGrant{{Key: "gog", Volume: "vol-gog"}}, ServerSecrets: map[string]string{"GOG_KEYRING_PASSWORD": "keyring-pw"}}), false)
	refused(seen)
	if !strings.Contains(seen.secrets, "mcp-image-gog.gog_keyring_password=keyring-pw") {
		t.Errorf("the image server's secret did not reach the gateway:\n%s", seen.secrets)
	}

	// The Coding agent: each stored secret by name, each git host's token under
	// the variable of its place among the hosts in order, and the dependency
	// databases only with its commands on.
	coding := segment(Coder(), Grants{
		Coder:   &spec.CoderGrant{Commands: true, DependencyScan: true},
		Secrets: map[string]string{"B_KEY": "bee", "A_KEY": "ay"},
		Accounts: []Account{
			{CoderAccount: spec.CoderAccount{Host: "gitlab.com", Kind: "gitlab", User: "grace", Name: "Grace", Email: "grace@example.com"}, Token: "tok-gitlab"},
			{CoderAccount: spec.CoderAccount{Host: "github.com", Kind: "github", User: "ada", Name: "Ada", Email: "ada@example.com"}, Token: "tok-github"},
		},
	})
	seen = startAndSee(t, coding, true)
	refused(seen)
	for _, want := range []string{"agentry-tools.a_key=ay\n", "agentry-tools.b_key=bee\n", "agentry-tools.agentry_account_token_1=tok-github\n", "agentry-tools.agentry_account_token_2=tok-gitlab\n"} {
		if !strings.Contains(seen.secrets, want) {
			t.Errorf("the Coding agent's secrets lack %q:\n%s", want, seen.secrets)
		}
	}
	if _, err := os.Stat(osvdb.Dir(coding.Home)); err != nil || !strings.Contains(seen.catalog, osvdb.Dir(coding.Home)+":/run/agentry/osv-db:ro") {
		t.Errorf("the dependency databases were not readied and mounted (%v)", err)
	}
	granted, perr := spec.ParsePlan([]byte(seen.runPlan))
	if perr != nil || granted.Capabilities.Coder == nil {
		t.Fatalf("the run plan: %v", perr)
	}
	var hosts, tokenVars []string
	for _, a := range granted.Capabilities.Coder.Accounts {
		hosts, tokenVars = append(hosts, a.Host), append(tokenVars, a.TokenEnv)
	}
	if !slices.Equal(hosts, []string{"github.com", "gitlab.com"}) || !slices.Equal(tokenVars, []string{spec.AccountTokenEnv(0), spec.AccountTokenEnv(1)}) ||
		!slices.Equal(granted.Capabilities.Coder.Secrets, []string{"A_KEY", "B_KEY"}) {
		t.Errorf("the plan's accounts %v with variables %v and secrets %v", hosts, tokenVars, granted.Capabilities.Coder.Secrets)
	}
	noScan := segment(Coder(), Grants{Coder: &spec.CoderGrant{Commands: true}})
	seen = startAndSee(t, noScan, true)
	refused(seen)
	if _, err := os.Stat(osvdb.Dir(noScan.Home)); !os.IsNotExist(err) || strings.Contains(seen.catalog, "osv-db") {
		t.Errorf("the dependency databases were readied without the scan (%v)", err)
	}

	// Calendar feeds come in name order, each address and sign-in under the
	// variable of its place.
	feeds := segment(Assistant(), Grants{CalendarZone: "UTC", CalendarFeeds: []CalendarFeed{
		{Name: "Work", Host: "calendar.example.com", URL: "https://calendar.example.com/work.ics", Session: "work-session"},
		{Name: "Home", Host: "home.example.com", URL: "https://home.example.com/home.ics"},
	}})
	seen = startAndSee(t, feeds, false)
	refused(seen)
	for _, want := range []string{"agentry-tools.agentry_calendar_feed_1=https://home.example.com/home.ics\n", "agentry-tools.agentry_calendar_feed_2=https://calendar.example.com/work.ics\n",
		"agentry-tools.agentry_calendar_session_2=work-session\n"} {
		if !strings.Contains(seen.secrets, want) {
			t.Errorf("the calendar secrets lack %q:\n%s", want, seen.secrets)
		}
	}
	if strings.Contains(seen.secrets, "agentry_calendar_session_1") {
		t.Errorf("a feed with no sign-in was given one:\n%s", seen.secrets)
	}
	if planned, perr := spec.ParsePlan([]byte(seen.runPlan)); perr != nil || planned.Capabilities.Calendar == nil {
		t.Fatalf("the plan's calendar: %v", perr)
	} else if c := planned.Capabilities.Calendar; c.TimeZone != "UTC" || len(c.Feeds) != 2 ||
		c.Feeds[0] != (spec.CalendarFeed{Name: "Home", Host: "home.example.com", URLEnv: spec.CalendarFeedEnv(0)}) ||
		c.Feeds[1] != (spec.CalendarFeed{Name: "Work", Host: "calendar.example.com", URLEnv: spec.CalendarFeedEnv(1), SessionEnv: spec.CalendarSessionEnv(1)}) {
		t.Errorf("the plan's calendar %+v", c)
	}

	// A grant the agent does not declare, or that is not one, is refused as the
	// policy's.
	policyRefusal := func(name string, seg Segment, want string) {
		t.Helper()
		_, err := seg.Plan(context.Background(), io.Discard)
		var ce *engine.ClassifiedError
		if !errors.As(err, &ce) || ce.Code != engine.ExitPolicy || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want a policy refusal saying %q", name, err, want)
		}
	}
	policyRefusal("calendar feeds to the Coding agent", segment(Coder(), Grants{CalendarFeeds: []CalendarFeed{{Name: "Work", Host: "calendar.example.com", URL: "https://calendar.example.com/work.ics"}}}),
		"is not given calendar feeds: it does not declare capabilities.persistent.calendars")
	policyRefusal("a calendar feed on a loopback name", segment(Assistant(), Grants{CalendarFeeds: []CalendarFeed{{Name: "Work", Host: "localhost", URL: "https://localhost/work.ics"}}}), "localhost")
	policyRefusal("the Coding agent's grant to the Assistant", segment(Assistant(), Grants{Coder: &spec.CoderGrant{Commands: true}}),
		"is not given the coding toolchain: it does not declare capabilities.persistent.coder")

	// The share's servers are the roster's, a copy of it, and only with the
	// share on.
	roster := []string{"image:a", "image:b"}
	if got := segment(Assistant(), Grants{ShareServers: roster}).shareServers(); got != nil {
		t.Errorf("share servers with the share off: %v", got)
	}
	got := segment(Assistant(), Grants{Share: "/share", ShareServers: roster}).shareServers()
	if !slices.Equal(got, roster) {
		t.Fatalf("share servers %v, want %v", got, roster)
	}
	got[0] = "image:changed"
	if roster[0] != "image:a" {
		t.Error("the share servers alias the roster's")
	}

	// An agent that pays with Agentry credits hands its agent container the
	// session's token.
	credits := segment(Assistant(), Grants{})
	credits.Policy.Model = policy.Model{Provider: policy.ProviderAgentry, Name: "agentry", BaseURL: "https://registry.example.test" + policy.AgentryInferencePath, ProxyAllow: []string{"registry.example.test"}}
	credits.Inference = &engine.InferenceGrant{Session: "sess-1", Token: "agym-session-token", Model: "agentry-model"}
	seen = startAndSee(t, credits, false)
	refused(seen)
	if seen.agent.Env[adapter.AgentryInferenceTokenEnv] != "agym-session-token" {
		t.Errorf("the agent's %s = %q, want the session's token", adapter.AgentryInferenceTokenEnv, seen.agent.Env[adapter.AgentryInferenceTokenEnv])
	}

	// A published agent that declares the web tools, the review grant or a
	// voice's use is given each key and voice the roster turns on.
	published := func(keys string, g Grants) Segment {
		home := t.TempDir()
		rel := layInstalledManifest(t, home, "registry", trust.StateVerified, guidedManifest("capabilities:\n  persistent:\n"+keys))
		seg := segment(Builtin{}, g)
		seg.Home, seg.Release = home, &rel
		return seg
	}
	seen = startAndSee(t, published("    web: true\n", Grants{Web: &engine.WebRun{Scan: toolsrv.WebScanJev, JevKey: "jev-key"}}), false)
	refused(seen)
	if !strings.Contains(seen.toolspec, `"web"`) || !strings.Contains(seen.secrets, "jev-key") {
		t.Errorf("the web tools were not given their keys:\n%s\n%s", seen.toolspec, seen.secrets)
	}
	seen = startAndSee(t, published("    review: true\n", Grants{Review: &engine.ReviewRun{JevKey: "review-key"}}), false)
	refused(seen)
	if !strings.Contains(seen.secrets, "review-key") {
		t.Errorf("the review grant was not given its key:\n%s", seen.secrets)
	}
	voice, verr := engine.AttachVoice([]byte("# Voice\n\nShort and plain.\n"), "kept on this computer")
	if verr != nil {
		t.Fatal(verr)
	}
	seen = startAndSee(t, published("", Grants{Voice: voice}), false)
	refused(seen)
	if !slices.Contains(seen.packs, engine.VoiceChannelDir) {
		t.Errorf("the voice was not staged for the segment: %v", seen.packs)
	}
}

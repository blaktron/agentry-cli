package persistent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// remoteServer is an MCP server that needs no package and no network to
// compile.
var remoteServer = manifest.MCPServer{URL: "https://mcp.example.com/mcp", Auth: manifest.AuthNone}

// fakePacks opens each named pack as an attached pack of that name.
func fakePacks(opened *[]string) Packs {
	return func(_ context.Context, names []string) ([]engine.AttachedPack, func(), error) {
		var out []engine.AttachedPack
		for _, n := range names {
			*opened = append(*opened, n)
			out = append(out, engine.AttachedPack{Name: n, Digest: "sha256:" + n})
		}
		return out, func() {}, nil
	}
}

// configured is the Assistant's config with a pack and a server added and
// everything agreed to.
func configured(t *testing.T, home string) (Agent, Config) {
	t.Helper()
	a := assistantAgent(t)
	c := Config{Enabled: true}
	for _, ch := range []Change{{AddKnowledge: "notes"}, {AddServer: &remoteServer}} {
		ch.Consent = true
		if _, err := ApplyChange(home, a, &c, ch); err != nil {
			t.Fatal(err)
		}
	}
	return a, c
}

// TestShareFolderRefusesDangerousPaths: a share folder is refused inside or
// around the runner's folders or the configuration's, through a link, at or
// beneath a system folder, and as a relative path; a folder of the person's
// own passes, made or not.
func TestShareFolderRefusesDangerousPaths(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(userHome, ".config"))
	home := filepath.Join(userHome, ".agentry", "runner")
	for _, d := range []string{home, filepath.Join(userHome, ".config", "agentry"), filepath.Join(userHome, "docs"), filepath.Join(userHome, ".ssh")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(userHome, "link")
	if err := os.Symlink(filepath.Join(userHome, "docs"), link); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{filepath.Join(userHome, "docs"), filepath.Join(userHome, "agentry", "assistantshare")} {
		if err := CheckShareFolder(home, ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"the runner home":         filepath.Join(home, "share"),
		"a folder holding it":     filepath.Join(userHome, ".agentry"),
		"the home folder":         userHome,
		"the configuration":       filepath.Join(userHome, ".config", "agentry", "x"),
		"the ssh keys":            filepath.Join(userHome, ".ssh"),
		"a link":                  link,
		"beneath a link":          filepath.Join(link, "sub"),
		"the root":                "/",
		"a system folder":         "/etc",
		"beneath a system folder": "/usr/local/share/x",
		"a relative path":         "share",
		"an unclean path":         filepath.Join(userHome, "docs") + "/../docs",
	} {
		if err := CheckShareFolder(home, bad); err == nil {
			t.Errorf("%s (%s) was accepted", name, bad)
		}
	}
}

// TestRosterRendersAgree: the consent a change asks for, the receipt's
// roster block and the roster a client lists are the same entries with the
// same sentences.
func TestRosterRendersAgree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := assistantAgent(t)
	c := Config{Enabled: true}
	_, err := ApplyChange(home, a, &c, Change{AddKnowledge: "notes"})
	var ce *ConsentError
	if !errors.As(err, &ce) {
		t.Fatalf("adding a pack without consent: %v", err)
	}
	r := BuildRoster(a, c, nil)
	r2 := BuildRoster(a, Config{Knowledge: []string{"notes"}}, nil)
	for _, e := range ce.Entries {
		got, ok := r2.Entry(e.ID)
		if !ok || got.Line != e.Line || !strings.Contains(ce.Error(), e.Line) {
			t.Errorf("the consent's %s is not the roster's: %+v vs %+v", e.ID, e, got)
		}
	}
	if len(r) != 22 || r[0].ID != EntryShare || r[1].ID != EntryData || !r[1].Fixed || r[2].ID != EntryMemory || !r[2].Fixed || !r[2].On ||
		r[3].ID != EntryHeartbeat || r[3].On || r[4].ID != EntryMeetings || r[4].On {
		t.Fatalf("the Assistant's own roster: %+v", r)
	}
	for _, e := range r[5:] {
		if e.Kind != KindSkill || e.GrantedBy != GrantedByBuiltin {
			t.Errorf("the Assistant's own roster ends in %+v, want its built-in skills", e)
		}
	}
	rec := r2.Receipt()
	if len(rec) != len(r2) {
		t.Fatalf("the receipt has %d entries, the roster %d", len(rec), len(r2))
	}
	for i, e := range r2 {
		if rec[i].ID != e.ID || rec[i].Line != e.Line || rec[i].On != e.On || rec[i].Kind != string(e.Kind) || rec[i].GrantedBy != e.GrantedBy {
			t.Errorf("receipt entry %d %+v, roster %+v", i, rec[i], e)
		}
	}
}

// TestRosterReachesEveryGate: each entry switched on or off reaches the
// segment's plan as one: the share's bind and write lists and the tools
// server's folders, a server's catalogue entry and its tools in the plan,
// and a pack among what the segment is given to attach.
func TestRosterReachesEveryGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a, c := configured(t, home)
	for _, tc := range []struct {
		name                 string
		share, server, packs bool
	}{
		{"everything on", true, true, true},
		{"the share off", false, true, true},
		{"the server off", true, false, true},
		{"the pack off", true, true, false},
		{"everything off", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := c
			cfg.Switches = map[string]bool{EntryShare: tc.share, ServerEntryID(remoteServer): tc.server, "knowledge:notes": tc.packs}
			RecordConsent(BuildRoster(a, cfg, nil), &cfg)
			var opened []string
			g, applied, done, err := Apply(context.Background(), home, a, cfg, BuildRoster(a, cfg, nil), fakePacks(&opened), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer done()
			if (g.Share != "") != tc.share || (len(g.Servers) == 1) != tc.server || (len(g.Knowledge) == 1) != tc.packs || slices.Equal(opened, []string{"notes"}) != tc.packs {
				t.Fatalf("grants %+v, packs opened %v", g, opened)
			}
			if slices.Contains(applied.On, EntryShare) != tc.share || slices.Contains(applied.On, "knowledge:notes") != tc.packs {
				t.Errorf("applied %v", applied.On)
			}
			sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: g}.Plan(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			plan := sp.Plan
			af := plan.Capabilities.AgentFiles
			if af == nil || af.Share != tc.share || af.CheckShare != tc.share {
				t.Fatalf("the plan's folders: %+v", af)
			}
			cat, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "catalog.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var roots, writable string
			for _, e := range plan.Gateway.Env {
				switch e.Name {
				case "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS":
					roots = e.Value
				case "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS":
					writable = e.Value
				}
			}
			share := strings.Contains(string(cat), spec.PHShare+":") && strings.Contains(roots, spec.PHShare) && strings.Contains(writable, spec.PHShare)
			if share != tc.share {
				t.Errorf("the share in the catalogue and both bind lists: %v, want %v", share, tc.share)
			}
			ts, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "toolspec.json"))
			if err != nil {
				t.Fatal(err)
			}
			allow, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "tools.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(ts), `"shareChecks"`) != tc.share || strings.Contains(string(ts), `"share"`) != tc.share {
				t.Errorf("the tools spec's share folder and checks: %s", ts)
			}
			for _, tool := range []string{"fs_read", "fs_write", "grep"} {
				if !strings.Contains(string(allow), tool) || !slices.Contains(plan.Tools, tool) {
					t.Errorf("%s is not in the allowlist and the plan's tools", tool)
				}
			}
			served := len(plan.MCPServers) == 1 && strings.Contains(string(cat), plan.MCPServers[0].Name+":") && slices.Contains(plan.Gateway.Servers, plan.MCPServers[0].Name)
			if served != tc.server {
				t.Errorf("the server in the plan, the catalogue and the gateway's servers: %v, want %v", served, tc.server)
			}
		})
	}
}

// TestRosterReachesEveryGateWithTheServerShare: a server's share switch
// reaches, by Apply, the server's catalogue entry's volumes, the bind list,
// the server's consent sentence and the segment's receipt together; with
// the share off, the server sees nothing whatever its switch says.
func TestRosterReachesEveryGateWithTheServerShare(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	for _, a := range []Agent{assistantAgent(t), coderAgent(t)} {
		record(t, serversDir(home, a.Name), wordServer)
		c := Config{Enabled: true}
		if _, err := ApplyChange(home, a, &c, Change{AddServer: &wordServer, Consent: true}); err != nil {
			t.Fatal(err)
		}
		// The Assistant's server sees the mirror, the Coding agent's the
		// folder itself.
		how, src, line := spec.ServerShareMirror, spec.PHShareMirror, ", and sees your share folder (read only, and writes new files into it)"
		if a.Builtin.Codes() {
			how, src, line = spec.ServerShareLive, spec.PHShare, ", and sees your share folder (read only, unchecked, and writes new files into it)"
		}
		for _, tc := range []struct {
			name        string
			share, sees bool
		}{
			{"the server sees the share", true, true},
			{"its switch off", true, false},
			{"the share off", false, true},
		} {
			t.Run(a.Name+": "+tc.name, func(t *testing.T) {
				cfg := c
				cfg.Switches = map[string]bool{EntryShare: tc.share, ServerShareID(wordServer): tc.sees}
				r := BuildRoster(a, cfg, nil)
				RecordConsent(r, &cfg)
				e, _ := r.Entry(ServerEntryID(wordServer))
				if e.Share != tc.sees || strings.HasSuffix(e.Line, line) != tc.sees || e.ShareSwitch != ServerShareID(wordServer) ||
					!strings.HasSuffix(e.ShareLine, line) || tc.sees && e.ShareLine != e.Line {
					t.Fatalf("the server's entry %+v", e)
				}
				rec := r.Receipt()
				if i := slices.IndexFunc(rec, func(x receipt.RosterEntry) bool { return x.ID == e.ID }); i < 0 || rec[i].Share != tc.sees || rec[i].Line != e.Line {
					t.Fatalf("the receipt's roster %+v", rec)
				}
				g, _, done, err := Apply(context.Background(), home, a, cfg, r, nil, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer done()
				sees := tc.share && tc.sees
				if len(g.Servers) != 1 || (len(g.ShareServers) == 1) != sees {
					t.Fatalf("grants: servers %v, share servers %v", g.Servers, g.ShareServers)
				}
				sp, err := localrun.Segment{Builtin: a.Builtin, Home: home, Policy: policy.Default(), PolicySHA: "s", Build: "test", Grants: g}.Plan(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				cat, err := os.ReadFile(filepath.Join(sp.Dir, "gateway", "catalog.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				roots, writable := "", ""
				for _, ev := range sp.Plan.Gateway.Env {
					switch ev.Name {
					case "MCP_GATEWAY_DOCKER_BIND_ALLOWED_PATHS":
						roots = ev.Value
					case "MCP_GATEWAY_DOCKER_BIND_ALLOW_WRITABLE_PATHS":
						writable = ev.Value
					}
				}
				bind := src + ":" + spec.AgentRoot + "/" + spec.AgentShareDir + ":ro"
				if strings.Contains(string(cat), bind) != sees || (sp.Plan.MCPServers[0].Share == how) != sees ||
					how == spec.ServerShareMirror && strings.Contains(roots, spec.PHShareMirror) != sees {
					t.Errorf("the share in the catalogue, the bind roots and the plan's server: want %v\n%s\nroots %s", sees, cat, roots)
				}
				// The share outbox, the server's one writable folder of the
				// share's: in its catalogue entry, a bind root and an exact
				// writable path of the gateway's, and the receipt's line.
				outbox := spec.PHShareOut + ":" + spec.AgentRoot + "/" + spec.AgentShareOutDir
				if strings.Contains(string(cat), outbox) != sees || slices.Contains(strings.Split(roots, ":"), spec.PHShareOut) != sees ||
					slices.Contains(strings.Split(writable, ":"), spec.PHShareOut) != sees ||
					strings.Contains(sp.Plan.Enforcement["persistent.serverShare"], spec.AgentRoot+"/"+spec.AgentShareOutDir) != sees {
					t.Errorf("the share outbox: want %v\n%s\nroots %s\nwritable %s\nreceipt %q", sees, cat, roots, writable, sp.Plan.Enforcement["persistent.serverShare"])
				}
			})
		}
	}
}

// TestAReadOnlyAgreementAsksAgainForTheOutbox: an agreement to a server's
// sentence that it sees the share without saving into it covers the server
// with its switch off, and not with it on, which says it writes new files
// into the share.
func TestAReadOnlyAgreementAsksAgainForTheOutbox(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	for _, a := range []Agent{assistantAgent(t), coderAgent(t)} {
		c := Config{Enabled: true}
		if _, err := ApplyChange(home, a, &c, Change{AddServer: &wordServer, Consent: true}); err != nil {
			t.Fatal(err)
		}
		c.Consented = map[string]string{EntryShare: c.Consented[EntryShare], ServerEntryID(wordServer): serverLine(wordServer) + readOnlyShareLine(a)}
		for _, e := range Unconsented(BuildRoster(a, c, nil), c) {
			if e.ID == ServerEntryID(wordServer) {
				t.Fatalf("%s: the server with its switch off asks again", a.Name)
			}
		}
		c.Switches = map[string]bool{ServerShareID(wordServer): true}
		asked := false
		for _, e := range Unconsented(BuildRoster(a, c, nil), c) {
			asked = asked || e.ID == ServerEntryID(wordServer) && strings.HasSuffix(e.Line, "and writes new files into it)")
		}
		if !asked {
			t.Fatalf("%s: a server that writes into the share was taken as agreed", a.Name)
		}
	}
}

// TestShareRefusedForAServerWithEgress: a server that reaches the network,
// a remote server and one that signs in by OAuth cannot be switched to see
// the share, and have no switch for it; switching it off is refused nothing.
func TestShareRefusedForAServerWithEgress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	a := assistantAgent(t)
	egress := manifest.MCPServer{NPM: "@example/fetcher", Version: "1.0.0", Egress: []string{"api.example.com"}}
	for _, srv := range []manifest.MCPServer{egress, remoteServer} {
		c := Config{Enabled: true}
		if _, err := ApplyChange(home, a, &c, Change{AddServer: &srv, Consent: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: ServerShareID(srv), On: true}, Consent: true}); err == nil {
			t.Errorf("%s was switched to see the share", serverName(srv))
		}
		if c.Switches[ServerShareID(srv)] {
			t.Errorf("a refused switch was kept for %s", serverName(srv))
		}
		if e, _ := BuildRoster(a, c, nil).Entry(ServerEntryID(srv)); e.ShareSwitch != "" || e.Share {
			t.Errorf("%s offers a share switch: %+v", serverName(srv), e)
		}
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: ServerShareID(srv), On: false}}); err != nil {
			t.Errorf("switching it off: %v", err)
		}
		// A switch set some other way gives it nothing.
		c.Switches = map[string]bool{ServerShareID(srv): true}
		r := BuildRoster(a, c, nil)
		RecordConsent(r, &c)
		g, _, done, err := Apply(context.Background(), home, a, c, r, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		done()
		if len(g.ShareServers) != 0 {
			t.Errorf("%s was given the share: %v", serverName(srv), g.ShareServers)
		}
	}
	if _, err := ApplyChange(home, a, &Config{}, Change{Switch: &Switch{ID: "server:npm-nothing:share", On: true}}); err == nil {
		t.Error("a server the agent does not have was switched")
	}
}

// TestServerShareLineIsConsented: letting a server see the share changes its
// sentence, so it asks again, and the sentence says the server sees the
// share read only (for the Coding agent, unchecked); taking it away asks
// nothing, and giving it back as agreed asks nothing either. Removing the
// server takes its switch with it.
func TestServerShareLineIsConsented(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home := shortHome(t)
	for _, a := range []Agent{assistantAgent(t), coderAgent(t)} {
		c := Config{Enabled: true}
		if _, err := ApplyChange(home, a, &c, Change{AddServer: &wordServer, Consent: true}); err != nil {
			t.Fatal(err)
		}
		on := Change{Switch: &Switch{ID: ServerShareID(wordServer), On: true}}
		_, err := ApplyChange(home, a, &c, on)
		var ce *ConsentError
		if !errors.As(err, &ce) || len(ce.Entries) != 1 || ce.Entries[0].ID != ServerEntryID(wordServer) {
			t.Fatalf("%s: letting the server see the share: %v", a.Name, err)
		}
		want := ", and sees your share folder (read only, and writes new files into it)"
		if a.Builtin.Codes() {
			want = ", and sees your share folder (read only, unchecked, and writes new files into it)"
		}
		if !strings.HasSuffix(ce.Entries[0].Line, want) {
			t.Fatalf("%s: the sentence asked for: %q", a.Name, ce.Entries[0].Line)
		}
		if c.Switches[ServerShareID(wordServer)] {
			t.Fatal("a refused switch was kept")
		}
		on.Consent = true
		if _, err := ApplyChange(home, a, &c, on); err != nil || !c.Switches[ServerShareID(wordServer)] || c.Consented[ServerEntryID(wordServer)] != ce.Entries[0].Line {
			t.Fatalf("agreed: %v, config %+v", err, c)
		}
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: ServerShareID(wordServer), On: false}}); err != nil {
			t.Fatalf("taking it away asked: %v", err)
		}
		for _, e := range Unconsented(BuildRoster(a, c, nil), c) {
			if e.ID == ServerEntryID(wordServer) {
				t.Fatal("the narrower sentence is not agreed to")
			}
		}
		if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: ServerShareID(wordServer), On: true}}); err != nil {
			t.Fatalf("giving it back as agreed asked: %v", err)
		}
		if _, err := ApplyChange(home, a, &c, Change{RemoveServer: ServerEntryID(wordServer)}); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Switches[ServerShareID(wordServer)]; ok {
			t.Error("the removed server's share switch was kept")
		}
	}
}

// TestChangeAsksForConsentWhenItWidens: turning on what the person has not
// agreed to as it reads is refused until they agree; turning it off and on
// again as it was asks nothing, and choosing another share folder asks
// again.
func TestChangeAsksForConsentWhenItWidens(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := shortHome(t)
	a, c := configured(t, home)
	var ce *ConsentError
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: "knowledge:notes", On: false}}); err != nil {
		t.Fatalf("switching off: %v", err)
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: "knowledge:notes", On: true}}); err != nil {
		t.Fatalf("switching back on what was agreed to: %v", err)
	}
	other := filepath.Join(userHome, "elsewhere")
	if _, err := ApplyChange(home, a, &c, Change{Share: &other}); !errors.As(err, &ce) || ce.Entries[0].ID != EntryShare {
		t.Fatalf("another share folder: %v", err)
	}
	if c.Share != "" {
		t.Fatal("a refused change was applied")
	}
	if _, err := ApplyChange(home, a, &c, Change{Share: &other, Consent: true}); err != nil || c.Share != other {
		t.Fatalf("agreed: %v, share %q", err, c.Share)
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: EntryData, On: false}}); err == nil {
		t.Error("the agent's own files were switched off")
	}
	oauth := manifest.MCPServer{URL: "https://mcp.example.org/mcp", Auth: manifest.AuthOAuth}
	if _, err := ApplyChange(home, a, &c, Change{AddServer: &oauth, Consent: true}); err != nil {
		t.Fatalf("adding an OAuth server: %v", err)
	}
	if e, _ := BuildRoster(a, c, nil).Entry(ServerEntryID(oauth)); e.State != StateNeedsSignIn || e.On {
		t.Errorf("an OAuth server is %+v, want it off and needing a sign-in", e)
	}
	if _, err := ApplyChange(home, a, &c, Change{Switch: &Switch{ID: ServerEntryID(oauth), On: true}, Consent: true}); err == nil {
		t.Error("an OAuth server was switched on")
	}
}

// TestASwitchTakesEffectAtTheNextTurn: a change to a running agent's roster
// leaves its segment running and pending; the next turn ends it, starts the
// next with the change, keeps the chat attached and sends the turn there;
// each segment's envelope records its own roster.
func TestASwitchTakesEffectAtTheNextTurn(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv := startAssistant(t, home, st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events, detach := sv.Subscribe()
	defer detach()
	if err := sv.Send(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	r, err := sv.Change(Change{Switch: &Switch{ID: EntryShare, On: false}})
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := r.Entry(EntryShare); e.State != StatePending || !sv.Status().Pending {
		t.Fatalf("the switch is %+v, pending %v", e, sv.Status().Pending)
	}
	if len(st.segments) != 1 {
		t.Fatal("a change started a segment before the next turn")
	}
	if err := sv.Send(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if len(st.segments) != 2 || st.segments[0].Grants.Share == "" || st.segments[1].Grants.Share != "" {
		t.Fatalf("segments %d; shares %q then %q", len(st.segments), st.segments[0].Grants.Share, st.segments[1].Grants.Share)
	}
	if got := st.last.sentTurns(); len(got) != 1 || !strings.HasSuffix(got[0], "second") || !strings.HasPrefix(got[0], "(The person changed what you have") {
		t.Errorf("the next segment was sent %q", got)
	}
	on := func(seg localrun.Segment) bool {
		for _, e := range seg.Persistent.Roster {
			if e.ID == EntryShare {
				return e.On
			}
		}
		return false
	}
	if !on(st.segments[0]) || on(st.segments[1]) {
		t.Error("the segments' envelopes do not record the roster each had")
	}
	var kinds []session.Kind
	applying := false
	for len(kinds) < 32 {
		select {
		case ev := <-events:
			kinds = append(kinds, ev.Kind)
			applying = applying || ev.Kind == session.KindNotice && strings.HasPrefix(ev.Text, "Applying your change")
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	if !applying || slices.Contains(kinds, session.KindSessionEnd) {
		t.Errorf("the chat saw %v: want the change applied and no end", kinds)
	}
	if sv.Status().Segment != 2 || sv.Status().Pending {
		t.Errorf("status %+v", sv.Status())
	}
	if _, err := sv.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	u, _ := LoadUsage(home, "assistant")
	if u.Segments != 2 {
		t.Errorf("usage counts %d segments", u.Segments)
	}
}

// TestNameAndPersonalityReachTheNextTurn: a name and a personality the person
// gives a running agent are quoted at the start of its next turn, once, and
// the agent is not restarted for them.
func TestNameAndPersonalityReachTheNextTurn(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	st := &starter{resumed: true}
	sv := startAssistant(t, home, st)
	defer func() { _, _ = sv.Stop(context.Background()) }()
	ctx := context.Background()
	name, personality := "Sandra", "Warm, \"brief\" and dry."
	if _, err := sv.Change(Change{Name: &name, Personality: &personality}); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	sent := st.last.sentTurns()
	if len(st.segments) != 1 || len(sent) != 2 {
		t.Fatalf("segments %d, turns %q", len(st.segments), sent)
	}
	if !strings.Contains(sent[0], `"Sandra"`) || !strings.Contains(sent[0], `"Warm, \"brief\" and dry."`) || !strings.HasSuffix(sent[0], "hello") {
		t.Errorf("the first turn after the change: %q", sent[0])
	}
	if sent[1] != "again" {
		t.Errorf("the turn after: %q", sent[1])
	}
	long := strings.Repeat("x", MaxPersonalityBytes+1)
	if _, err := sv.Change(Change{Personality: &long}); err == nil {
		t.Error("a personality past the bound was accepted")
	}
	bad := "two\nlines"
	if _, err := sv.Change(Change{Name: &bad}); err == nil {
		t.Error("a name of two lines was accepted")
	}
}

// TestEveryNextSegmentIsHeldToConsent: a config changed some other way, to
// turn on what the person has not agreed to, is refused at the start and at
// the next segment alike, and the agent stops saying why.
func TestEveryNextSegmentIsHeldToConsent(t *testing.T) {
	home := shortHome(t)
	enable(t, home)
	c, _ := LoadConfig(home, "assistant")
	c.Knowledge = []string{"notes"}
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	var ce *ConsentError
	if _, err := Start(context.Background(), Options{Home: home, Agent: assistantAgent(t), Client: ClientCLI, Policy: policy.Default(), StartSegment: (&starter{}).start}); !errors.As(err, &ce) {
		t.Fatalf("start with an unagreed pack: %v", err)
	}
	c.Knowledge = nil
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	st := &starter{resumed: true}
	sv := startAssistant(t, home, st)
	c, _ = LoadConfig(home, "assistant")
	c.Knowledge = []string{"notes"}
	if err := SaveConfig(home, "assistant", c); err != nil {
		t.Fatal(err)
	}
	if err := sv.Send(context.Background(), "hello"); !errors.As(err, &ce) {
		t.Fatalf("the next segment with an unagreed pack: %v", err)
	}
	select {
	case <-sv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the agent kept running on a refused segment")
	}
	if len(st.segments) != 1 {
		t.Errorf("%d segments started", len(st.segments))
	}
}

// TestCheckOffAsksNothing: the share's sentence says what the malware-check
// setting makes true, ClamAV's check with it on and an unchecked read with
// it off, and changing the setting asks nothing: an agreement to the share
// under either setting is an agreement to it under the other. The Coding
// agent's share reads raw whatever the setting.
func TestCheckOffAsksNothing(t *testing.T) {
	home := t.TempDir()
	a := assistantAgent(t)
	c := Config{Enabled: true}
	RecordConsent(BuildRosterUnder(a, c, nil, true), &c)
	off, _ := BuildRosterUnder(a, c, nil, false).Entry(EntryShare)
	on, _ := BuildRosterUnder(a, c, nil, true).Entry(EntryShare)
	if !strings.Contains(on.Line, "checked for malware with ClamAV") || strings.Contains(on.Line, "unchecked:") {
		t.Errorf("the share's sentence with the check on: %q", on.Line)
	}
	if !strings.Contains(off.Line, "the malware check is off on this computer") || strings.Contains(off.Line, "checked for malware with ClamAV") {
		t.Errorf("the share's sentence with the check off: %q", off.Line)
	}
	for _, mc := range []bool{false, true} {
		if pending := Unconsented(BuildRosterUnder(a, c, nil, mc), c); len(pending) > 0 {
			t.Errorf("check %v asks again: %+v", mc, pending)
		}
	}
	// Agreed with the check off, then turned on: nothing is asked either,
	// and a change made under the new setting widens nothing.
	c = Config{Enabled: true}
	RecordConsent(BuildRosterUnder(a, c, nil, false), &c)
	if pending := Unconsented(BuildRosterUnder(a, c, nil, true), c); len(pending) > 0 {
		t.Errorf("turning the check on asks again: %+v", pending)
	}
	if _, err := applyChange(home, a, &c, Change{Switch: &Switch{ID: EntryShare, On: true}}, true); err != nil {
		t.Errorf("a change under the other setting: %v", err)
	}
	// A sentence the person never agreed to is still asked about.
	c.Consented[EntryShare] = "Shares the folder elsewhere"
	if pending := Unconsented(BuildRosterUnder(a, c, nil, true), c); len(pending) != 1 {
		t.Errorf("an agreement to another sentence passed: %+v", pending)
	}
	coder := coderAgent(t)
	for _, mc := range []bool{false, true} {
		if e, _ := BuildRosterUnder(coder, Config{}, nil, mc).Entry(EntryShare); !strings.Contains(e.Line, "with no malware check") {
			t.Errorf("the Coding agent's share with the check %v: %q", mc, e.Line)
		}
	}
}

// TestServerLineNamesItsArgs: a server started with arguments says so in
// its consent sentence, as written, so changing them is agreed to again;
// one with none reads as before.
func TestServerLineNamesItsArgs(t *testing.T) {
	srv := manifest.MCPServer{PyPI: "semgrep", Version: "1.182.0", Egress: []string{"semgrep.dev"}, Args: []string{"mcp"}}
	if got, want := serverLine(srv), "Uses the MCP server pypi:semgrep@1.182.0 started with the arguments mcp, all its tools, reaching semgrep.dev alone"; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	srv.Args = nil
	if got := serverLine(srv); strings.Contains(got, "arguments") {
		t.Fatalf("a server with no args names some: %q", got)
	}
}

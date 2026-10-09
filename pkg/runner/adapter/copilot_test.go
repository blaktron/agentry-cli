package adapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/adapter/copilottest"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

func copilotInput(mode manifest.Mode) adapter.Input {
	return adapter.Input{
		Mode:         mode,
		Instruction:  "You are a QuickAgent.",
		Model:        policy.Model{Provider: policy.ProviderCopilot, Name: "auto", APIKeyKind: policy.KeyKindCopilotSignIn, ProxyAllow: policy.SignInProxyAllow(policy.KeyKindCopilotSignIn)},
		HarnessImage: "agentry-copilot:1.0.95",
		GatewayURL:   "http://gateway:8811/mcp",
	}
}

// A Copilot run compiles to Copilot CLI's server mode with every built-in
// and every ambient source off, the gateway its one MCP server, no key, no
// token and no mount: its sign-in is handed over the session (I1, I3, I4).
func TestTheCopilotImageCompilesASignInRun(t *testing.T) {
	ad, err := adapter.Get(adapter.CopilotImageAdapterName)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []manifest.Mode{manifest.ModeAutonomous, manifest.ModeInteractive} {
		res, err := ad.Compile(copilotInput(mode))
		if err != nil {
			t.Fatal(err)
		}
		a := res.Agent
		argv := strings.Join(a.Argv, " ")
		for _, want := range []string{"--headless --stdio", "--no-auto-update", "--no-auto-login", "--disable-builtin-mcps",
			"--no-custom-instructions", "--no-sandbox", "--no-remote", "--available-tools mcp:*"} {
			if !strings.Contains(argv, want) {
				t.Errorf("argv %q lacks %q", argv, want)
			}
		}
		if a.Entrypoint != "/usr/local/bin/copilot" || !a.ReadOnlyRoot || len(a.Mounts) != 0 {
			t.Fatalf("container: %+v", a)
		}
		env := map[string]string{}
		for _, e := range a.Env {
			if e.FromHostEnv != "" || e.FromKeyring != "" || e.FromRunToken || e.FromModelSignIn {
				t.Fatalf("the container is given %s from outside the plan", e.Name)
			}
			env[e.Name] = e.Value
		}
		for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "GITHUB_COPILOT_API_TOKEN", "COPILOT_API_URL", spec.GatewayTokenEnv} {
			if _, ok := env[name]; ok {
				t.Fatalf("the container's environment names %s", name)
			}
		}
		if env["COPILOT_DISABLE_KEYTAR"] != "1" || env["COPILOT_PKG_CACHE_HOME"] != "/opt/copilot/cache" || env["HTTPS_PROXY"] != spec.ProxyURL() {
			t.Fatalf("env %v", env)
		}
		msi := res.ModelSignIn
		if msi == nil || msi.Kind != policy.KeyKindCopilotSignIn || msi.Delivery != spec.ModelSignInCallback || msi.Path != "" || msi.Env != "" {
			t.Fatalf("model sign-in %+v", msi)
		}
		var cfg map[string]any
		if err := json.Unmarshal(res.Files["session.json"], &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg["enableSessionTelemetry"] != false || cfg["skipCustomInstructions"] != true || cfg["requestPermission"] != true ||
			cfg["enableSkills"] != false || cfg["model"] != "auto" || !slices.Equal(anyStrings(cfg["availableTools"]), []string{"mcp:*"}) {
			t.Fatalf("session.json %s", res.Files["session.json"])
		}
		servers, _ := cfg["mcpServers"].(map[string]any)
		gw, _ := servers["gateway"].(map[string]any)
		if len(servers) != 1 || gw["url"] != "http://gateway:8811/mcp" || gw["headers"] != nil {
			t.Fatalf("mcpServers %v", servers)
		}
		if string(res.Files["instruction.md"]) != "You are a QuickAgent." {
			t.Fatalf("instruction %q", res.Files["instruction.md"])
		}
		if (mode == manifest.ModeInteractive) != (res.Session != nil) || (mode == manifest.ModeAutonomous) != (res.OneTurn != nil) {
			t.Fatalf("%s: session %v one turn %v", mode, res.Session, res.OneTurn)
		}
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// The image runs on a Copilot sign-in and on nothing else, and takes no
// base URL.
func TestCopilotsImageRunsOnNothingElse(t *testing.T) {
	ad, _ := adapter.Get(adapter.CopilotImageAdapterName)
	in := copilotInput(manifest.ModeAutonomous)
	in.Model = policy.Model{Provider: "openai", Name: "gpt-5.2", APIKeyEnv: "OPENAI_API_KEY"}
	var r *adapter.Refusal
	if _, err := ad.Compile(in); !errors.As(err, &r) {
		t.Fatalf("a key: %v", err)
	}
	in = copilotInput(manifest.ModeAutonomous)
	in.Model.BaseURL = "https://example.test/v1"
	if _, err := ad.Compile(in); !errors.As(err, &r) {
		t.Fatalf("a base URL: %v", err)
	}
	if got := adapter.DefaultFor(&policy.Policy{Model: copilotInput(manifest.ModeAutonomous).Model}); got != adapter.CopilotImageAdapterName {
		t.Fatalf("default for a Copilot plan: %s", got)
	}
}

// driven is a Copilot session against a fake server: the driver, the
// server, and the events, collected until the driver closes.
type driven struct {
	drv    adapter.SessionDriver
	srv    *copilotest
	events func() []adapter.Event
}

type copilotest = copilottest.Server

type pipeRW struct {
	io.Reader
	io.WriteCloser
}

func drive(t *testing.T, srv *copilottest.Server, opts adapter.SessionOptions) driven {
	t.Helper()
	toSrvR, toSrvW := io.Pipe()
	fromSrvR, fromSrvW := io.Pipe()
	go func() {
		_ = srv.Serve(toSrvR, fromSrvW)
		_ = fromSrvW.Close()
	}()
	ad, _ := adapter.Get(adapter.CopilotImageAdapterName)
	if opts.HarnessFile == nil {
		res, err := ad.Compile(copilotInput(manifest.ModeInteractive))
		if err != nil {
			t.Fatal(err)
		}
		opts.HarnessFile = func(name string) ([]byte, error) {
			b, ok := res.Files[name]
			if !ok {
				return nil, errors.New("no such file")
			}
			return b, nil
		}
	}
	drv, err := ad.Session(context.Background(), pipeRW{Reader: fromSrvR, WriteCloser: toSrvW}, opts)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var evs []adapter.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range drv.Events() {
			mu.Lock()
			evs = append(evs, ev)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = drv.Close(); <-done })
	return driven{drv: drv, srv: srv, events: func() []adapter.Event {
		_ = drv.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(evs)
	}}
}

// tokens is a sign-in's token source that counts its renewals.
type tokens struct {
	mu      sync.Mutex
	asked   []bool
	expires time.Time
	err     error
}

func (k *tokens) get(_ context.Context, refresh bool) (adapter.SignInToken, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, refresh)
	if k.err != nil {
		return adapter.SignInToken{}, k.err
	}
	tok := "ghu_initialtoken0000"
	if refresh {
		tok = "ghu_renewedtoken0000"
	}
	return adapter.SignInToken{AccessToken: tok, ExpiresAt: k.expires}, nil
}

func ofKind(evs []adapter.Event, kind adapter.EventKind) []adapter.Event {
	var out []adapter.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// The session hands Copilot CLI the run's token through its callback at
// session.create and a renewed one when it asks for a refresh, the gateway
// its bearer in the session alone, and the run's instruction as the whole
// system message; a gateway tool of the grant is approved once and any
// other refused; the turn's events reach the runner's envelope; and no
// event carries a token or the bearer.
func TestACopilotSessionIsHandedItsTokenThroughTheCallback(t *testing.T) {
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	srv := &copilottest.Server{Turn: func(c *copilottest.Conn, prompt string) {
		if prompt != "go" {
			c.Event("session.error", map[string]any{"errorType": "query", "message": "unexpected prompt " + prompt})
			return
		}
		c.AskToken("refresh")
		c.ToolCall("c1", "write_report")
		c.ToolCall("c2", "delete_everything")
		c.Event("assistant.usage", map[string]any{"model": "claude-sonnet-5.5", "inputTokens": 10, "outputTokens": 5, "cacheReadTokens": 3})
		c.Event("assistant.message", map[string]any{"messageId": "a1", "content": "DONE"})
		c.Event("session.idle", map[string]any{})
	}}
	d := drive(t, srv, adapter.SessionOptions{GatewayTools: []string{"write_report"}, ModelSignIn: true, SignInToken: k.get, GatewayToken: "gateway-bearer-0000"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	turn, err := d.drv.Prompt(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "DONE" || turn.Usage == nil || turn.Usage.InputTokens != 10 || turn.Usage.OutputTokens != 5 {
		t.Fatalf("turn %+v", turn)
	}
	evs := d.events()
	create, options, toks, decisions, _ := srv.Snapshot()

	if !slices.Equal(k.asked, []bool{false, true}) {
		t.Fatalf("token source asked %v", k.asked)
	}
	if len(toks) != 2 || toks[0]["accessToken"] != "ghu_initialtoken0000" || toks[1]["accessToken"] != "ghu_renewedtoken0000" ||
		toks[0]["kind"] != "token" || toks[0]["expiresIn"].(float64) < 7*3600 {
		t.Fatalf("tokens %v", toks)
	}
	sys, _ := create["systemMessage"].(map[string]any)
	if sys["mode"] != "replace" || sys["content"] != "You are a QuickAgent." || create["enableSessionTelemetry"] != false {
		t.Fatalf("create %v", create)
	}
	gw := create["mcpServers"].(map[string]any)["gateway"].(map[string]any)
	if gw["headers"].(map[string]any)["Authorization"] != "Bearer gateway-bearer-0000" {
		t.Fatalf("gateway %v", gw)
	}
	if options["skipCustomInstructions"] != true || len(options["installedPlugins"].([]any)) != 0 {
		t.Fatalf("options %v", options)
	}
	if decisions["p-c1"]["kind"] != "approve-once" || decisions["p-c2"]["kind"] != "reject" {
		t.Fatalf("decisions %v", decisions)
	}
	if g, dn := ofKind(evs, adapter.EventPermissionGranted), ofKind(evs, adapter.EventPermissionDenied); len(g) != 1 || g[0].Text != "write_report" || len(dn) != 1 || dn[0].Text != "delete_everything" {
		t.Fatalf("granted %v denied %v", g, dn)
	}
	if calls := ofKind(evs, adapter.EventToolCall); len(calls) != 2 || calls[0].Text != "write_report" {
		t.Fatalf("tool calls %v", calls)
	}
	if u := ofKind(evs, adapter.EventUsage); len(u) != 1 || u[0].InputTokens != 10 || u[0].OutputTokens != 5 || u[0].CostUSD != 0 {
		t.Fatalf("usage %v", u)
	}
	for _, ev := range evs {
		for _, secret := range []string{"ghu_initialtoken0000", "ghu_renewedtoken0000", "gateway-bearer-0000"} {
			if strings.Contains(ev.Text, secret) || strings.Contains(string(ev.Raw), secret) {
				t.Fatalf("an event carries %s: %+v", secret, ev)
			}
		}
	}
}

// A run's token source that fails refuses the callback, and with no sign-in
// at all the session is never created: Copilot CLI never falls back to an
// ambient login.
func TestACopilotSessionWithoutItsTokenIsNotCreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	k := &tokens{err: errors.New("could not renew")}
	d := drive(t, &copilottest.Server{}, adapter.SessionOptions{ModelSignIn: true, SignInToken: k.get})
	if err := d.drv.Start(ctx); err == nil {
		t.Fatal("started without a token")
	}
	d2 := drive(t, &copilottest.Server{}, adapter.SessionOptions{ModelSignIn: true})
	if err := d2.drv.Start(ctx); err == nil || !strings.Contains(err.Error(), "Not authenticated") {
		t.Fatalf("started with no token source: %v", err)
	}
}

// On a run paid by a sign-in, a quota or a rate-limit error is the plan's
// limit, said once, and the turn fails; any other error is an error.
func TestACopilotQuotaOrRateLimitIsThePlansLimit(t *testing.T) {
	for typ, code := range map[string]string{"quota": "quota_exceeded", "rate_limit": "user_weekly_rate_limited", "query": ""} {
		srv := &copilottest.Server{Turn: func(c *copilottest.Conn, _ string) {
			c.Event("session.error", map[string]any{"errorType": typ, "errorCode": code, "message": "You have reached your limit.", "statusCode": 429})
			c.Event("session.error", map[string]any{"errorType": typ, "errorCode": code, "message": "You have reached your limit.", "statusCode": 429})
			c.Event("session.idle", map[string]any{})
		}}
		k := &tokens{expires: time.Now().Add(8 * time.Hour)}
		d := drive(t, srv, adapter.SessionOptions{ModelSignIn: true, SignInToken: k.get})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := d.drv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := d.drv.Prompt(ctx, "go")
		cancel()
		if err == nil {
			t.Fatalf("%s: a turn that ended on an error succeeded", typ)
		}
		evs := d.events()
		limits := ofKind(evs, adapter.EventPlanLimit)
		if typ == "query" {
			if len(limits) != 0 || len(ofKind(evs, adapter.EventError)) == 0 {
				t.Fatalf("query: %v", evs)
			}
			continue
		}
		if len(limits) != 1 || !strings.Contains(limits[0].Text, code) || !limits[0].ResetsAt.IsZero() {
			t.Fatalf("%s: plan limits %v", typ, limits)
		}
	}
}

// An organization's policy that turns Copilot CLI off for the account
// refuses session.create with the policy named; managed settings that
// require a person to approve each tool call refuse the call and end the
// run with that policy named; GitHub refusing the token says to sign in
// again.
func TestAnOrganizationsPolicyIsNamed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}

	d := drive(t, &copilottest.Server{CreateError: "Error: Access denied by policy settings"}, adapter.SessionOptions{ModelSignIn: true, SignInToken: k.get})
	var refused *adapter.OrgPolicyRefusal
	if err := d.drv.Start(ctx); !errors.As(err, &refused) || refused.Policy != receipt.OrgPolicyClient || !strings.Contains(refused.Text, "Access denied by policy settings") {
		t.Fatalf("the CLI turned off: %v", err)
	}

	d = drive(t, &copilottest.Server{CreateError: `GitHub token validation failed: Failed to fetch Copilot user info: 401 Unauthorized: {"message":"Bad credentials"}`},
		adapter.SessionOptions{ModelSignIn: true, SignInToken: k.get})
	if err := d.drv.Start(ctx); err == nil || !strings.Contains(err.Error(), "sign in again with agentry model-signins add copilot") || errors.As(err, &refused) {
		t.Fatalf("a refused token: %v", err)
	}

	srv := &copilottest.Server{Turn: func(c *copilottest.Conn, _ string) {
		c.AskPermission("p1", map[string]any{"kind": "mcp", "serverName": "gateway", "toolName": "write_report", "managedApprovalRequired": true})
		c.Event("session.error", map[string]any{"errorType": "query", "message": "The tool call was rejected."})
	}}
	d = drive(t, srv, adapter.SessionOptions{GatewayTools: []string{"write_report"}, ModelSignIn: true, SignInToken: k.get})
	if err := d.drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = d.drv.Prompt(ctx, "go")
	evs := d.events()
	_, _, _, decisions, _ := srv.Snapshot()
	pol := ofKind(evs, adapter.EventOrgPolicy)
	if decisions["p1"]["kind"] != "reject" || len(pol) != 1 || pol[0].Policy != receipt.OrgPolicyManagedApproval {
		t.Fatalf("managed approval: decisions %v, events %v", decisions, pol)
	}
}

// Anything else Copilot CLI asks of the driver is refused.
func TestACopilotRequestTheDriverDoesNotServeIsRefused(t *testing.T) {
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	var answer map[string]any
	srv := &copilottest.Server{Turn: func(c *copilottest.Conn, _ string) {
		answer = c.Ask("userInput.request", map[string]any{"question": "?"})
		c.Event("session.idle", map[string]any{})
	}}
	d := drive(t, srv, adapter.SessionOptions{ModelSignIn: true, SignInToken: k.get})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.drv.Prompt(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	if answer["error"] == nil {
		t.Fatalf("answered %v", answer)
	}
}

// A tool that is not the gateway's starting at all (a built-in the
// allowlist should have unloaded) ends the session: the turn fails, the
// session is aborted, and no turn is taken after it (I1).
func TestACopilotToolOutsideTheGatewayEndsTheSession(t *testing.T) {
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	srv := &copilottest.Server{Turn: func(c *copilottest.Conn, _ string) {
		c.Event("tool.execution_start", map[string]any{"toolCallId": "c9", "toolName": "bash", "arguments": map[string]any{"command": "env"}})
		c.Event("session.idle", map[string]any{})
	}}
	d := drive(t, srv, adapter.SessionOptions{GatewayTools: []string{"write_report"}, ModelSignIn: true, SignInToken: k.get})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.drv.Prompt(ctx, "go"); err == nil || !strings.Contains(err.Error(), "not the gateway's (bash)") {
		t.Fatalf("turn: %v", err)
	}
	if _, err := d.drv.Prompt(ctx, "again"); err == nil {
		t.Fatal("a turn after the breach")
	}
	evs := d.events()
	_, _, _, _, methods := srv.Snapshot()
	if !slices.Contains(methods, "session.abort") || len(ofKind(evs, adapter.EventError)) == 0 {
		t.Fatalf("methods %v", methods)
	}
}

// The session's allowlist, telemetry switch and approval request are the
// driver's own, whatever the plan's session.json says, and its gateway must
// be the run's.
func TestTheCopilotSessionHoldsItsAllowlistWhateverThePlanSays(t *testing.T) {
	ad, _ := adapter.Get(adapter.CopilotImageAdapterName)
	res, err := ad.Compile(copilotInput(manifest.ModeInteractive))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal(res.Files["session.json"], &cfg)
	cfg["availableTools"] = []string{"builtin:*", "mcp:*"}
	cfg["enableSessionTelemetry"] = true
	loose, _ := json.Marshal(cfg)
	files := func(name string) ([]byte, error) {
		if name == "session.json" {
			return loose, nil
		}
		return res.Files[name], nil
	}
	k := &tokens{expires: time.Now().Add(8 * time.Hour)}
	srv := &copilottest.Server{}
	d := drive(t, srv, adapter.SessionOptions{ModelSignIn: true, SignInToken: k.get, HarnessFile: files})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.drv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	create, _, _, _, _ := srv.Snapshot()
	if !slices.Equal(anyStrings(create["availableTools"]), []string{"mcp:*"}) || create["enableSessionTelemetry"] != false {
		t.Fatalf("create %v", create)
	}

	cfg["mcpServers"] = map[string]any{"gateway": map[string]any{"type": "http", "url": "https://elsewhere.example/mcp", "tools": []string{"*"}}}
	loose, _ = json.Marshal(cfg)
	d2 := drive(t, &copilottest.Server{}, adapter.SessionOptions{ModelSignIn: true, SignInToken: k.get, HarnessFile: files, GatewayToken: "bearer-0000"})
	if err := d2.drv.Start(ctx); err == nil || !strings.Contains(err.Error(), "must name the run's gateway") {
		t.Fatalf("another gateway URL: %v", err)
	}
}

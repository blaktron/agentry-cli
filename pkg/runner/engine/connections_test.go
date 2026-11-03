package engine

// A package server fed by a connection, and the renewal of a run's
// connections before they expire: the server's token folder holds its
// templates rendered with the access token and nothing else; an env token
// reaches the gateway's secrets file alone and is fixed for the run; a
// renewed token rewrites the folder by rename, is masked from then on, and
// nothing renews after the run.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

const (
	packageAccessToken = "ya29.first-access-token-0123456789"
	renewedAccessToken = "ya29.renewed-access-token-9876543210"
	workspaceTemplate  = `{"token": "{access_token}", "client_id": "agentry", "client_secret": "agentry", "expiry": "{expiry}", "expiry_date": {expiry_ms}}`
)

func withPackageOAuthServer(t *testing.T, p Params, expires time.Time) (Params, spec.MCPServerSpec) {
	t.Helper()
	srv := spec.MCPServerSpec{
		Name: "mcp-pypi-workspace-mcp", Source: "pypi",
		Package: "workspace-mcp", Version: "2.0.1",
		Image: "python:3.12-alpine", StoreSubdir: "pypi-workspace-mcp",
		Egress: []string{"gmail.googleapis.com"},
		OAuth: &spec.OAuthSpec{
			Issuer: "https://accounts.google.com",
			Files:  []spec.OAuthFile{{Path: "me.json", Template: workspaceTemplate}},
			Env: map[string]string{
				"WORKSPACE_MCP_CREDENTIALS_DIR": "{dir}",
				"GOOGLE_ACCESS_TOKEN":           "{access_token}",
			},
		},
	}
	plan := *p.Plan
	plan.MCPServers = []spec.MCPServerSpec{srv}
	p.Plan = &plan
	p.ServersDir = t.TempDir()
	p.Connections = map[string]ConnectionToken{"pypi:workspace-mcp": {AccessToken: packageAccessToken, ExpiresAt: expires}}
	return p, srv
}

// lockedBuffer is the run's stderr, read while the renewer may write.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestAPackageServerReadsItsTokenFolderAndARenewalRewritesIt(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	expires := time.Now().Add(3 * time.Second).Truncate(time.Second)
	p, srv := withPackageOAuthServer(t, p, expires)
	var stderr lockedBuffer
	p.Stderr = &stderr
	renewed := make(chan struct{})
	var renewals atomic.Int32
	p.RenewConnection = func(_ context.Context, server string) (ConnectionToken, error) {
		if server != "pypi:workspace-mcp" {
			t.Errorf("renewal asked for %q", server)
		}
		if renewals.Add(1) == 1 {
			defer close(renewed)
		}
		return ConnectionToken{AccessToken: renewedAccessToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	cfg := filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg")
	tokenPath := filepath.Join(cfg, spec.ConnectionsDir, srv.Name, "me.json")
	var before, after, secretsFile string
	var mode, dirMode os.FileMode
	f.Foreground["-agent"].Before = func(dockerx.RunSpec) {
		b, _ := os.ReadFile(tokenPath) //nolint:gosec // the test's run dir
		before = string(b)
		if fi, err := os.Stat(filepath.Dir(tokenPath)); err == nil {
			dirMode = fi.Mode().Perm()
		}
		b, _ = os.ReadFile(filepath.Join(cfg, "gateway", "secrets.env")) //nolint:gosec // the test's run dir
		secretsFile = string(b)
		if fi, err := os.Stat(tokenPath); err == nil {
			mode = fi.Mode().Perm()
		}
		select {
		case <-renewed:
		case <-time.After(10 * time.Second):
			t.Error("the run's token was not renewed before it expired")
		}
		// The renewal's file write follows its call; wait for it.
		for range 100 {
			if b, _ = os.ReadFile(tokenPath); strings.Contains(string(b), renewedAccessToken) { //nolint:gosec // the test's run dir
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		after = string(b)
	}
	f.LogsByName["agentry-cafe0000-gateway"] = "server said " + renewedAccessToken + " and " + packageAccessToken + "\n"

	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v (%+v)", err, env)
	}
	want := `{"token": "` + packageAccessToken + `", "client_id": "agentry", "client_secret": "agentry", "expiry": "` +
		expires.UTC().Format(time.RFC3339) + `", "expiry_date": ` + strconv.FormatInt(expires.UnixMilli(), 10) + `}`
	if before != want || mode != 0o600 {
		t.Fatalf("the token file holds\n%s\n(mode %v), want\n%s", before, mode, want)
	}
	if runtime.GOOS != "windows" && dirMode != 0o700 {
		t.Fatalf("the token folder is mode %v, want 0700", dirMode)
	}
	if !strings.Contains(after, `"token": "`+renewedAccessToken+`"`) || strings.Contains(after, packageAccessToken) {
		t.Fatalf("after the renewal the token file holds\n%s", after)
	}
	// The env token is in the gateway's secrets file under the server's
	// key, and the folder's path is not a secret.
	if !strings.Contains(secretsFile, "mcp-pypi-workspace-mcp.google_access_token="+packageAccessToken+"\n") ||
		strings.Contains(secretsFile, "WORKSPACE_MCP_CREDENTIALS_DIR") || strings.Contains(secretsFile, "workspace_mcp_credentials_dir") {
		t.Fatalf("secrets.env:\n%s", secretsFile)
	}
	// Both tokens are masked in the receipts: the renewed one from the
	// moment it was taken.
	b, err := os.ReadFile(filepath.Join(env.Receipts.Dir, "gateway.log"))
	if err != nil || strings.Contains(string(b), renewedAccessToken) || strings.Contains(string(b), packageAccessToken) {
		t.Fatalf("gateway.log = %q (%v)", b, err)
	}
	if _, err := os.Stat(filepath.Join(cfg, spec.ConnectionsDir)); !os.IsNotExist(err) {
		t.Fatalf("the connection folders outlived the run: %v", err)
	}
	// Nothing renews after the run: the renewer stopped with it.
	n := renewals.Load()
	time.Sleep(50 * time.Millisecond)
	if renewals.Load() != n || n != 1 {
		t.Fatalf("renewals: %d, then %d", n, renewals.Load())
	}
	for _, c := range f.Calls {
		blob := c.Arg + " " + strings.Join(c.Run.Argv, " ")
		for k, v := range c.Run.Env {
			blob += " " + k + "=" + v
		}
		if strings.Contains(blob, packageAccessToken) || strings.Contains(blob, renewedAccessToken) {
			t.Fatalf("a docker call carries an access token: %+v", c)
		}
	}
	if strings.Contains(stderr.String(), renewedAccessToken) || strings.Contains(stderr.String(), packageAccessToken) {
		t.Fatalf("stderr carries a token:\n%s", stderr.String())
	}

	// A connection is renewed only where a new token would reach the server:
	// a package server that reads its token from the environment keeps the
	// one it started with, and nothing is renewed without the caller's renewal.
	rd := &runDirs{}
	envOnly := srv
	envOnly.OAuth = &spec.OAuthSpec{Env: map[string]string{"GOOGLE_ACCESS_TOKEN": "{access_token}"}}
	if h := heldConnection(p, rd, envOnly, time.Time{}); h.ask != nil || h.write != nil {
		t.Errorf("a token the server reads from its environment is renewable")
	}
	if h := heldConnection(p, rd, srv, time.Time{}); h.ask == nil || h.write == nil {
		t.Errorf("a token the server reads from a file is not renewable")
	}
	noRenewal := p
	noRenewal.RenewConnection = nil
	if h := heldConnection(noRenewal, rd, srv, time.Time{}); h.ask != nil || h.write != nil {
		t.Errorf("a connection is renewable without a renewal from the caller")
	}
}

// A renewal that fails is said once, without the token, and asked again;
// the next that works is said too and rewrites the bearer of a remote
// server's bridge.
func TestARefusedRenewalIsSaidAndAskedAgain(t *testing.T) {
	old := renewRetry
	renewRetry = 100 * time.Millisecond
	t.Cleanup(func() { renewRetry = old })
	p, f := fixture(t)
	happyFake(f)
	p, name := withOAuthServer(p)
	p.Connections[remoteServerKey] = ConnectionToken{AccessToken: remoteAccessToken, ExpiresAt: time.Now().Add(2 * time.Second)}
	var stderr lockedBuffer
	p.Stderr = &stderr
	var calls atomic.Int32
	worked := make(chan struct{})
	p.RenewConnection = func(context.Context, string) (ConnectionToken, error) {
		switch calls.Add(1) {
		case 1:
			return ConnectionToken{}, errors.New("the vendor refused (400, invalid_request) " + remoteAccessToken)
		case 2:
			// An answer with no token is a refusal too, and said only once.
			return ConnectionToken{ExpiresAt: time.Now().Add(time.Hour)}, nil
		case 3:
			defer close(worked)
		}
		return ConnectionToken{AccessToken: renewedAccessToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	bearer := filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg", spec.ConnectionsDir, name, spec.BearerFile)
	var got string
	var fileMode, dirMode os.FileMode
	f.Foreground["-agent"].Before = func(dockerx.RunSpec) {
		select {
		case <-worked:
		case <-time.After(10 * time.Second):
			t.Error("the renewal was not asked again")
		}
		for range 100 {
			if b, _ := os.ReadFile(bearer); string(b) == renewedAccessToken { //nolint:gosec // the test's run dir
				got = string(b)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if fi, err := os.Stat(bearer); err == nil {
			fileMode = fi.Mode().Perm()
		}
		if fi, err := os.Stat(filepath.Dir(bearer)); err == nil {
			dirMode = fi.Mode().Perm()
		}
	}
	if env, err := Run(context.Background(), f, p); err != nil || env.Status != "ok" {
		t.Fatalf("run: %v", err)
	}
	if got != renewedAccessToken {
		t.Fatalf("the bridge's bearer holds %q after the renewal", got)
	}
	if runtime.GOOS != "windows" && (fileMode != 0o600 || dirMode != 0o700) {
		t.Fatalf("the bridge's bearer is mode %v in a folder of mode %v, want 0600 in 0700", fileMode, dirMode)
	}
	out := stderr.String()
	if strings.Count(out, "could not renew the connection of MCP server https://mcp.linear.app/mcp") != 1 ||
		!strings.Contains(out, "renewed the connection of MCP server https://mcp.linear.app/mcp") ||
		strings.Contains(out, remoteAccessToken) || !strings.Contains(out, "(the vendor refused (400, invalid_request) [redacted])") {
		t.Fatalf("stderr:\n%s", out)
	}

	// A renewed token outside an access token's shape is not written where a
	// template reads it, and a renewal is not asked again sooner than the
	// retry, however short the token's life.
	renewal := func(shaped bool, token ConnectionToken) (*heldToken, []string, string, time.Time) {
		var wrote []string
		var note lockedBuffer
		h := &heldToken{
			what: "the test connection", shaped: shaped,
			ask:   func(context.Context) (ConnectionToken, error) { return token, nil },
			write: func(_ context.Context, t ConnectionToken) error { wrote = append(wrote, t.AccessToken); return nil },
		}
		before := time.Now()
		renewOne(context.Background(), Params{Stderr: &note}, &runDirs{}, h)
		return h, wrote, note.String(), before
	}
	odd := ConnectionToken{AccessToken: `tok", "refresh_token": "x`, ExpiresAt: time.Now().Add(time.Hour)}
	if h, wrote, note, _ := renewal(true, odd); len(wrote) != 0 || !h.failing || !strings.Contains(note, "not in the shape an access token takes") {
		t.Errorf("a package server's odd token: written %q, failing %v, note %q", wrote, h.failing, note)
	}
	if h, wrote, note, _ := renewal(false, odd); len(wrote) != 1 || h.failing || note != "" {
		t.Errorf("a remote server's bearer: written %q, failing %v, note %q", wrote, h.failing, note)
	}
	expired := ConnectionToken{AccessToken: renewedAccessToken, ExpiresAt: time.Now().Add(-time.Minute)}
	if h, _, _, before := renewal(false, expired); h.due.Before(before.Add(renewRetry)) {
		t.Errorf("a token already expired is asked for again at %s, sooner than %s after the last", h.due.Sub(before), renewRetry)
	}
}

// remoteServerKey is withOAuthServer's server, as Params.Connections keys it.
const remoteServerKey = "https://mcp.linear.app/mcp"

// A token that is not in an access token's shape is never written into a
// template, where a quote could add a key the server reads.
func TestAnAccessTokenOutsideItsShapeIsNotHandedOver(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p, srv := withPackageOAuthServer(t, p, time.Time{})
	p.Connections["pypi:workspace-mcp"] = ConnectionToken{AccessToken: `tok", "refresh_token": "x`}
	_, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if err == nil || !strings.Contains(err.Error(), "not in the shape an access token takes") || !errors.As(err, &ce) || ce.Code != ExitCLI {
		t.Fatalf("err = %v", err)
	}

	// The shape, token by token: RFC 6750's b64token, and nothing else.
	for _, tc := range []struct {
		token string
		ok    bool
	}{
		{"abc.DEF-1_2~3+4/5", true}, {"abc==", true}, {"a", true},
		{`a"b`, false}, {"a b", false}, {"a\nb", false}, {"abc\n", false}, {"a,b", false}, {"a;b", false}, {"=abc", false}, {"a=b", false},
	} {
		p.Connections["pypi:workspace-mcp"] = ConnectionToken{AccessToken: tc.token}
		if _, err := connectionToken(p, srv); (err == nil) != tc.ok {
			t.Errorf("token %q: err = %v, want accepted %v", tc.token, err, tc.ok)
		}
	}
	// A remote server's bearer travels as a header its client checks itself,
	// so any value reaches its bridge.
	remote, _ := withOAuthServer(p)
	remote.Connections = map[string]ConnectionToken{remoteServerKey: {AccessToken: "has a space"}}
	if _, err := connectionToken(remote, remote.Plan.MCPServers[0]); err != nil {
		t.Errorf("a remote server's bearer was held to the shape: %v", err)
	}
}

func TestAPackageServerWithNoConnectionRefusesTheRunByName(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p, _ = withPackageOAuthServer(t, p, time.Time{})
	for name, conns := range map[string]map[string]ConnectionToken{
		"none given":     nil,
		"an empty token": {"pypi:workspace-mcp": {}},
	} {
		p.Connections = conns
		env, err := Run(context.Background(), f, p)
		var ce *ClassifiedError
		if err == nil || !strings.Contains(err.Error(), "workspace-mcp@2.0.1") || !errors.As(err, &ce) || ce.Code != ExitPolicy {
			t.Fatalf("%s: err = %v, want exit %d naming the server", name, err, ExitPolicy)
		}
		wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonSecretMissing})
	}

	// A remote server is named as one.
	rp, rf := fixture(t)
	happyFake(rf)
	rp, _ = withOAuthServer(rp)
	rp.Connections = nil
	_, err := Run(context.Background(), rf, rp)
	if err == nil || !strings.Contains(err.Error(), "the remote MCP server "+remoteServerKey+" signs in by OAuth") {
		t.Fatalf("a remote server with no connection: %v", err)
	}
}

// An expiry the vendor did not say is read as an hour; a renewal is due
// half the remaining life before expiry, at most ten minutes before and at
// least a second.
func TestRenewAt(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		life, before time.Duration
	}{
		{time.Hour, 10 * time.Minute},
		{2 * time.Hour, 10 * time.Minute},
		{10 * time.Minute, 5 * time.Minute},
		{3 * time.Second, 1500 * time.Millisecond},
		{time.Second, time.Second},
		{-time.Minute, time.Second},
	} {
		at := renewAt(ConnectionToken{ExpiresAt: now.Add(c.life)}, now)
		if got := now.Add(c.life).Sub(at); got != c.before {
			t.Errorf("life %s: renews %s before expiry, want %s", c.life, got, c.before)
		}
	}
	if got := withExpiry(ConnectionToken{AccessToken: "x"}, now).ExpiresAt; !got.Equal(now.Add(time.Hour)) {
		t.Errorf("an unsaid expiry reads %s", got)
	}
}

// The redactor learns a value while others mask with it.
func TestTheRedactorLearnsARenewedTokenWhileMasking(t *testing.T) {
	r := newRedactor("gateway-token-abcdef", nil, nil)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				_ = r.String("x " + renewedAccessToken)
			}
		})
	}
	r.add(renewedAccessToken)
	r.add(renewedAccessToken)
	r.add("short")   // too short to be a credential
	r.add("seven77") // one short of the shortest value masked
	wg.Wait()
	if got := r.String("short and seven77 and short****"); got != "short and seven77 and short****" {
		t.Fatalf("a value below the shortest was masked: %q", got)
	}
	// The redactor holds each value whole and once in its partial form: two
	// tokens, four values, however often a token is added.
	if len(r.values) != 4 {
		t.Fatalf("the redactor holds %d values, want 4: %q", len(r.values), r.values)
	}
	if got := r.String("a " + renewedAccessToken + " b " + renewedAccessToken[:4] + "**** c gateway-token-abcdef"); got != "a [redacted] b [redacted] c [redacted]" {
		t.Fatalf("masked: %q", got)
	}
}

// A session renews its connections for as long as it lives, and stops
// renewing when it ends.
func TestASessionRenewsItsConnectionUntilItEnds(t *testing.T) {
	p, f := sessionFixture(t)
	happyFake(f)
	h := &fakeHarness{}
	f.Foreground["-agent"] = &dockerx.FakeForeground{Handler: h.handler}
	p, srv := withPackageOAuthServer(t, p, time.Now().Add(2*time.Second))
	var stderr lockedBuffer
	p.Stderr = &stderr
	renewed := make(chan struct{})
	var renewals atomic.Int32
	p.RenewConnection = func(context.Context, string) (ConnectionToken, error) {
		if renewals.Add(1) == 1 {
			defer close(renewed)
		}
		return ConnectionToken{AccessToken: renewedAccessToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	s, err := StartSession(context.Background(), f, p)
	if err != nil {
		t.Fatal(err)
	}
	_, done := collect(s)
	select {
	case <-renewed:
	case <-time.After(10 * time.Second):
		t.Fatal("the session's connection was not renewed")
	}
	var got, tokenPath string
	for range 100 {
		paths, _ := filepath.Glob(filepath.Join(p.RunRoot, "runs", "*", "cfg", spec.ConnectionsDir, srv.Name, "me.json"))
		if len(paths) == 1 {
			tokenPath = paths[0]
		}
		b, _ := os.ReadFile(tokenPath) //nolint:gosec // the test's run dir
		if got = string(b); strings.Contains(got, renewedAccessToken) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(got, `"token": "`+renewedAccessToken+`"`) {
		t.Fatalf("the session's token file holds %q", got)
	}
	if _, err := s.End(context.Background(), false); err != nil && !errors.Is(err, session.ErrEnded) {
		t.Fatal(err)
	}
	<-done
	n := renewals.Load()
	if _, err := os.Stat(filepath.Dir(tokenPath)); !os.IsNotExist(err) {
		t.Fatalf("the token folder outlived the session: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if renewals.Load() != n {
		t.Fatal("the renewer outlived the session")
	}
}

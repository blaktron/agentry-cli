package engine

// A run's OAuth connections. The caller hands the engine an access token
// per server that signs in, and nothing else of the connection. The engine
// writes it where the server reads it: a remote server's bridge reads
// cfg/connections/<server>/bearer, mounted read-only; a package server
// reads the files its oauth block declares, rendered into the same kind of
// folder, mounted writable, or an env entry the gateway sets from the
// secrets file. While the run lives, a renewer asks the caller for a new
// token before the one held expires and rewrites the folder; an env token
// is fixed for the run.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/blaktron/agentry/pkg/manifest"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// ConnectionToken is the part of a connection a run is given: an access
// token and when it expires (zero when the vendor did not say).
type ConnectionToken struct {
	AccessToken string
	ExpiresAt   time.Time
}

// assumedTokenLife stands in for an expiry the vendor did not say: what a
// template's {expiry} reads, and when the renewer asks again.
const assumedTokenLife = time.Hour

// accessTokenRE is RFC 6750's b64token, the shape of every access token
// the vendors issue. A token outside it is refused rather than
// written into a template, where a quote or a newline could add a key the
// server reads.
var accessTokenRE = regexp.MustCompile(`^[A-Za-z0-9\-._~+/]+=*$`)

// connectionValueKey is where a server's access token sits in the run's
// value map: a key no secrets-file key can be (those are
// "<server>.<env name>"), so it is masked and never written to
// secrets.env, which is built before it is added.
func connectionValueKey(serverName string) string { return "connection:" + serverName }

// connectionToken is the token the run was given for srv; a package
// server's, which a template or an env value carries, is held to an
// access token's shape (a bridge sends its bearer as a header, which the
// HTTP client checks itself). A missing one refuses the run (exit 11).
func connectionToken(p Params, srv spec.MCPServerSpec) (ConnectionToken, error) {
	t, ok := p.Connections[srv.ConnectionKey()]
	if !ok || t.AccessToken == "" {
		what := "remote MCP server " + srv.Package
		if !srv.Remote() {
			what = "MCP server " + srv.Ref()
		}
		return ConnectionToken{}, Classify(ExitPolicy, receipt.WithReason(fmt.Errorf("the %s signs in by OAuth and this run was given no connection for it: connect it first (agentry connect)", what),
			receipt.ReasonSecretMissing, ""))
	}
	if !srv.Remote() && !accessTokenRE.MatchString(t.AccessToken) {
		return ConnectionToken{}, Classify(ExitCLI, fmt.Errorf("the access token for MCP server %s is not in the shape an access token takes (RFC 6750), so it is not written into its files", srv.Ref()))
	}
	return t, nil
}

// tokenValues are a template's placeholders for t: the token, and its
// expiry as RFC 3339 UTC and as Unix milliseconds.
func tokenValues(t ConnectionToken) map[string]string {
	return map[string]string{
		"access_token": t.AccessToken,
		"expiry":       t.ExpiresAt.UTC().Format(time.RFC3339),
		"expiry_ms":    strconv.FormatInt(t.ExpiresAt.UnixMilli(), 10),
		"dir":          spec.ConnectionMount,
	}
}

// withExpiry fills in an expiry the vendor did not say.
func withExpiry(t ConnectionToken, now time.Time) ConnectionToken {
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = now.Add(assumedTokenLife)
	}
	return t
}

// connectionEnvValues renders each package server's oauth env entries that
// carry the access token, keyed as the gateway's secrets file reads them.
func connectionEnvValues(p Params) (map[string]string, error) {
	out := map[string]string{}
	for _, srv := range p.Plan.MCPServers {
		if srv.Remote() || srv.OAuth == nil {
			continue
		}
		var t ConnectionToken
		for name, v := range srv.OAuth.Env {
			if !srv.OAuth.TokenEnv(name) {
				continue
			}
			if t.AccessToken == "" {
				var err error
				if t, err = connectionToken(p, srv); err != nil {
					return nil, err
				}
			}
			r, err := manifest.ExpandOAuthEnvValue(v, tokenValues(t))
			if err != nil {
				return nil, Classify(ExitCLI, fmt.Errorf("MCP server %s: oauth env %s: %w", srv.Ref(), name, err))
			}
			out[compile.ServerSecretKey(srv.Name, name)] = r
		}
	}
	return out, nil
}

// writeConnectionFiles gives each server that signs in, and each remote
// server that sends a bearer, its credential: an auth: secret server's
// bearer, resolved as any declared secret is (values is secretValues' map,
// an optional one absent written empty); an auth: oauth server's access
// token; a package server's oauth files rendered with its access token.
// Each access token is also added to values so the run's redactor masks
// it. Everything is written into the server's own folder under the run's
// cfg, which the compiler mounts into that server alone (read-only into a
// bridge); the gateway never reads it. A file is replaced by a rename, so a reader
// mid-write sees the old value or the new one, which is how a renewed
// token is handed over (renewConnections).
func writeConnectionFiles(p Params, rd *runDirs, values map[string]string) error {
	bearers := map[string]spec.MCPServerSpec{}
	for key, server := range remoteBearerKeys(p.Plan) {
		bearers[key] = server
	}
	now := time.Now()
	rd.conns = nil
	for _, srv := range p.Plan.MCPServers {
		if !srv.SignsIn() {
			continue
		}
		t, err := connectionToken(p, srv)
		if err != nil {
			return err
		}
		t = withExpiry(t, now)
		key := connectionValueKey(srv.Name)
		values[key] = t.AccessToken
		if srv.Remote() {
			bearers[key] = srv
		} else if err := writeOAuthFiles(context.Background(), p, rd, srv, t, false); err != nil {
			return err
		}
		rd.conns = append(rd.conns, heldConnection(p, rd, srv, renewAt(t, now)))
	}
	if err := writeModelSignIn(p, rd, values); err != nil {
		return err
	}
	for key, server := range bearers {
		dir := filepath.Join(rd.connections(), server.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Classify(ExitCLI, fmt.Errorf("make the credential folder of the remote MCP server %s: %w", server.Package, err))
		}
		if err := writeBearer(dir, values[key], nil); err != nil {
			return Classify(ExitCLI, fmt.Errorf("write the credential of the remote MCP server %s: %w", server.Package, err))
		}
		relabelShared(context.Background(), p.Stderr, dir)
	}
	return nil
}

// writeBearer replaces a bridge's bearer file in dir by a rename, calling
// label on the new file first where given.
func writeBearer(dir, value string, label func(string)) error {
	return writeFileAtomic(filepath.Join(dir, spec.BearerFile), []byte(value), 0o600, label)
}

// writeOAuthFiles renders a package server's oauth files with t into its
// connection folder. On a renewal each file is labelled before it is
// renamed into place, so the server never meets a file it cannot read.
func writeOAuthFiles(ctx context.Context, p Params, rd *runDirs, srv spec.MCPServerSpec, t ConnectionToken, renewal bool) error {
	if len(srv.OAuth.Files) == 0 {
		return nil
	}
	dir := filepath.Join(rd.connections(), srv.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Classify(ExitCLI, fmt.Errorf("make the token folder of MCP server %s: %w", srv.Ref(), err))
	}
	vals := tokenValues(t)
	for _, f := range srv.OAuth.Files {
		body, err := manifest.ExpandOAuthTemplate(f.Template, vals)
		if err != nil {
			return Classify(ExitCLI, fmt.Errorf("MCP server %s: oauth file %s: %w", srv.Ref(), f.Path, err))
		}
		var label func(string)
		if renewal {
			label = func(tmp string) { relabelShared(ctx, p.Stderr, tmp) }
		}
		if err := writeFileAtomic(filepath.Join(dir, f.Path), []byte(body), 0o600, label); err != nil {
			return Classify(ExitCLI, fmt.Errorf("write the token file %s of MCP server %s: %w", f.Path, srv.Ref(), err))
		}
	}
	if !renewal {
		relabelShared(ctx, p.Stderr, dir)
	}
	return nil
}

// heldToken is one access token the run holds and renews: a server's
// connection (heldConnection) or the run's model sign-in's
// (writeModelSignIn). Each knows how to ask for its next token and where to
// write it; the renewer is the same for both.
type heldToken struct {
	// what names it in the run's notes.
	what string
	// ask is the caller's renewal; nil leaves the token as given.
	ask func(context.Context) (ConnectionToken, error)
	// write hands a renewed token over, labelling the new file before it
	// is renamed into place.
	write func(context.Context, ConnectionToken) error
	// shaped holds a renewed token to an access token's shape before it is
	// written where a template or a harness reads it.
	shaped bool
	// due is when the renewer asks for its next token, fixed when the
	// token is taken (or a renewal fails).
	due time.Time
	// failing is set once a renewal failed and said so, until one works.
	failing bool
}

// heldConnection is a server's connection as the run holds it: renewable
// when a renewed token would reach the server (a bridge reads its bearer
// at every request, a package server its files; an env token is fixed for
// the run), and only when the caller gave a renewal.
func heldConnection(p Params, rd *runDirs, srv spec.MCPServerSpec, due time.Time) *heldToken {
	h := &heldToken{what: "the connection of MCP server " + srv.Ref(), shaped: !srv.Remote(), due: due}
	if p.RenewConnection == nil || (!srv.Remote() && len(srv.OAuth.Files) == 0) {
		return h
	}
	h.ask = func(ctx context.Context) (ConnectionToken, error) { return p.RenewConnection(ctx, srv.ConnectionKey()) }
	h.write = func(ctx context.Context, t ConnectionToken) error {
		if srv.Remote() {
			return writeBearer(filepath.Join(rd.connections(), srv.Name), t.AccessToken,
				func(tmp string) { relabelShared(ctx, p.Stderr, tmp) })
		}
		return writeOAuthFiles(ctx, p, rd, srv, t, true)
	}
	return h
}

// renewAt is when to ask for a new token taken at now: half its remaining
// life before it expires, at most ten minutes before and at least a
// second. Ten minutes clears google-auth's own margin (it calls a token
// expired 3 min 45 s early), so an hour-long Google token is renewed well
// before a server reading its file thinks it has expired, and a short one
// at half its life.
func renewAt(t ConnectionToken, now time.Time) time.Time {
	margin := t.ExpiresAt.Sub(now) / 2
	margin = min(margin, 10*time.Minute)
	margin = max(margin, time.Second)
	return t.ExpiresAt.Add(-margin)
}

// renewRetry is how long the renewer waits after a refused or failed
// renewal before it asks again.
var renewRetry = time.Minute

// renewConnections starts the run's renewer once the redactor exists,
// and returns the function that stops it and waits for it; the caller
// registers that with teardown, which runs it before the connection
// folders are deleted. Nothing starts when the caller gave no
// RenewConnection or no connection would see a new token.
func renewConnections(ctx context.Context, p Params, rd *runDirs) (stop func()) {
	var held []*heldToken
	for _, h := range rd.conns {
		if h.ask != nil {
			held = append(held, h)
		}
	}
	if len(held) == 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			next := held[0].due
			for _, h := range held[1:] {
				if h.due.Before(next) {
					next = h.due
				}
			}
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			for _, h := range held {
				if !time.Now().Before(h.due) {
					renewOne(ctx, p, rd, h)
				}
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
	}
}

// renewOne asks for h's new token and hands it to the server. A failure is
// said once, on the run's stderr (masked), and asked again after
// renewRetry; the server's calls fail once the old token expires.
func renewOne(ctx context.Context, p Params, rd *runDirs, h *heldToken) {
	t, err := h.ask(ctx)
	if err == nil && t.AccessToken == "" {
		err = fmt.Errorf("the vendor sent no access token")
	}
	if err == nil && h.shaped && !accessTokenRE.MatchString(t.AccessToken) {
		err = fmt.Errorf("the vendor's new access token is not in the shape an access token takes")
	}
	if err == nil {
		// Masked before it is written anywhere a log could quote it.
		rd.redact.add(t.AccessToken)
		t = withExpiry(t, time.Now())
		err = h.write(ctx, t)
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		if !h.failing {
			_, _ = fmt.Fprintf(p.Stderr, "note: could not renew %s before its access token expires (%v); trying again until it can, and its calls fail once the token expires\n",
				h.what, rd.redact.Err(err))
			h.failing = true
		}
		h.due = time.Now().Add(renewRetry)
		return
	}
	if h.failing {
		_, _ = fmt.Fprintf(p.Stderr, "note: renewed %s\n", h.what)
		h.failing = false
	}
	// Never sooner than renewRetry, however short-lived the vendor's
	// tokens, so the renewer cannot press its token endpoint.
	now := time.Now()
	h.due = renewAt(t, now)
	if floor := now.Add(renewRetry); h.due.Before(floor) {
		h.due = floor
	}
}

package signin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/connection"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
)

// ConnectionVendor is the Kept.Vendor of an OAuth connection the runhost
// keeps for a release's server.
const ConnectionVendor = "connection"

// ConnectRequest asks the runhost to make a connection of its own to one of
// a release's servers: the person consents in the device's browser and the
// runhost keeps the grant it is issued, never a copy of the device's.
// Everything in it is what the consent needs; none of it is a token.
type ConnectRequest struct {
	// Ref, Digest and Server are the connection's key, as the runhost's
	// connection store and its schedules name it.
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	Server string `json:"server"`
	// URL is a remote server's address; Issuer, for a package server, the
	// authorization server its oauth block names. Exactly one is set.
	URL    string   `json:"url,omitempty"`
	Issuer string   `json:"issuer,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
	// AgentryClient is the client id of Agentry's own public client for the
	// server's issuer, which the device resolved; "" for none.
	AgentryClient string `json:"agentryClient,omitempty"`
	// OwnClientID and OwnClientSecret are the person's own OAuth client for
	// the issuer, where the vendor takes no other.
	OwnClientID     string `json:"ownClientId,omitempty"`
	OwnClientSecret string `json:"ownClientSecret,omitempty"`
}

// check refuses a request the consent cannot be made from.
func (r ConnectRequest) check() error {
	switch {
	case r.Ref == "" || r.Digest == "" || r.Server == "":
		return errors.New("a connection names no release or server")
	case (r.URL == "") == (r.Issuer == ""):
		return errors.New("a connection names a server's address or an issuer, and only one")
	case r.OwnClientID == "" && r.OwnClientSecret != "":
		return errors.New("an own OAuth client's secret without its id")
	}
	for _, u := range []string{r.URL, r.Issuer} {
		if u == "" {
			continue
		}
		p, err := url.Parse(u)
		if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil {
			return fmt.Errorf("%q is not an https address", u)
		}
	}
	return nil
}

func (r ConnectRequest) key() connection.Key {
	return connection.Key{Ref: r.Ref, Digest: r.Digest, Server: r.Server}
}

// Connect makes the connection the request on in's first line names, unless
// the runhost already keeps a working one, and answers with what it kept.
// The rest of in stays open while the consent runs; its end tells the
// runhost the device has left.
func (s *Server) Connect(ctx context.Context, in io.Reader, fw *runhost.FrameWriter) (int, error) {
	if s.Connections == nil {
		return engine.ExitUsage, errors.New("this runhost keeps no connections")
	}
	br := bufio.NewReaderSize(in, 64<<10)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return engine.ExitUsage, errors.New("the connection's request did not arrive")
	}
	var req ConnectRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return engine.ExitUsage, fmt.Errorf("the connection's request: %w", err)
	}
	if err := req.check(); err != nil {
		return engine.ExitUsage, err
	}
	store, clients := s.Connections()
	k := req.key()
	if c, err := store.Get(k); err == nil && c.Broken == "" {
		return 0, fw.Result(Kept{Vendor: ConnectionVendor, Kind: req.Server, Label: c.Issuer, KeptAt: c.CreatedAt})
	}
	id, err := newID()
	if err != nil {
		return 1, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a := &active{id: id, vendor: ConnectionVendor, ctx: ctx, slots: make(chan struct{}, MaxRelayed)}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		return engine.ExitUsage, errors.New("another sign-in is under way on this runhost; try again when it has finished")
	}
	s.active = a
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
	}()
	go func() {
		_, _ = io.Copy(io.Discard, br)
		cancel()
	}()
	var own *mcpauth.Client
	if req.OwnClientID != "" {
		own = &mcpauth.Client{ID: req.OwnClientID, Secret: req.OwnClientSecret, Kind: mcpauth.ClientOwn}
	}
	c := mcpauth.Consent{
		HTTP: s.connectHTTP(), ServerURL: req.URL, Issuer: req.Issuer, Scopes: req.Scopes,
		AgentryClient: func(context.Context, string) string { return req.AgentryClient },
		OwnClient: func(string) (*mcpauth.Client, error) {
			return own, nil
		},
		CIMDURL: s.CIMDURL,
		Say:     func(t string) { _ = fw.Out("%s", t) },
		Present: func(u string) {
			port, ok := loopbackRedirect(u, mcpauth.CallbackPath)
			if !ok {
				_ = fw.Out("the consent's address names no callback on the runhost's loopback; nothing can be relayed")
				return
			}
			s.mu.Lock()
			a.port = port
			s.mu.Unlock()
			_ = fw.Frame(openFrame(u, runhost.Open{Relay: a.id, Port: port}))
		},
	}
	g, err := c.Run(ctx)
	if err != nil {
		var needs *mcpauth.NeedsOwnClientError
		switch {
		case errors.As(err, &needs):
			return engine.ExitPolicy, err
		case ctx.Err() != nil && errors.Is(err, context.Canceled):
			return 1, errors.New("nothing kept: the device left before the consent finished")
		}
		if k, ok := connection.ConnectorAtIssuer(req.Issuer); ok && req.AgentryClient != "" {
			if step := connection.AdminStep(err, k, req.AgentryClient, req.Scopes); step != "" {
				_ = fw.Out("%s", step)
				return exitOf(err), fmt.Errorf("an administrator must approve Agentry's app for your organization first: %w", err)
			}
		}
		return exitOf(err), err
	}
	if own != nil {
		if err := clients.Set(g.Discovery.Server.Issuer, own.ID, own.Secret); err != nil {
			return 1, err
		}
	}
	// A broken connection kept before is replaced: one made with the same
	// client is not revoked, since a vendor that revokes a client's whole
	// grant would end the new one too.
	if old, err := store.Get(k); err == nil && old != nil {
		if old.MadeWith(g) {
			store.RecordReplaced(old, "replaced by a new consent for the runhost")
		} else {
			_, _ = store.Remove(ctx, k, s.connectHTTP(), clients, "replaced by a new consent for the runhost")
		}
	}
	kept, err := store.Put(k, g, "")
	if err != nil {
		return 1, err
	}
	return 0, fw.Result(Kept{Vendor: ConnectionVendor, Kind: req.Server, Label: kept.Issuer, KeptAt: kept.CreatedAt})
}

func (s *Server) connectHTTP() *http.Client {
	if s.ConnectHTTP != nil {
		return s.ConnectHTTP()
	}
	return mcpauth.Guard{}.Client()
}

// Connect makes the runhost's own connection req names, with the consent in
// the person's browser on this device, and answers with what the runhost
// kept. authorize is the authorization endpoint the device discovered for
// the server itself: the device opens no other page. Nothing of the grant
// reaches this device.
func Connect(ctx context.Context, c Caller, req ConnectRequest, authorize string, p Person) (Kept, error) {
	if err := req.check(); err != nil {
		return Kept{}, err
	}
	line, err := json.Marshal(req)
	if err != nil {
		return Kept{}, err
	}
	page := func(addr string, o runhost.Open) bool { return consentPage(authorize, addr, o) }
	k, err := call(ctx, c, []string{ConnectionVendor}, append(line, '\n'), page, false, p)
	if err != nil {
		return Kept{}, err
	}
	if k.Vendor != ConnectionVendor || k.Kind != req.Server {
		return Kept{}, errors.New("the runhost did not say what it kept")
	}
	return k, nil
}

// consentPage says whether addr is a consent at the authorization endpoint
// authorize, redirecting to a loopback callback at the open frame's port.
func consentPage(authorize, addr string, o runhost.Open) bool {
	want, err := url.Parse(authorize)
	if err != nil || want.Scheme != "https" {
		return false
	}
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "https" || u.Host != want.Host || strings.TrimSuffix(u.Path, "/") != strings.TrimSuffix(want.Path, "/") || u.User != nil {
		return false
	}
	port, ok := loopbackRedirect(addr, mcpauth.CallbackPath)
	return ok && o.Relay != "" && o.Port == port && o.Code == ""
}

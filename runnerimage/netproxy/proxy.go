// Package netproxy is the llm-proxy of the agent-container topology
// (RUNNER-PLAN §R4): the run network is created --internal, so the ONLY
// way anything the agent container runs can reach the outside world is a
// CONNECT tunnel through this proxy — and the proxy tunnels only to the
// operator's model endpoints (policy.model.proxyAllow). Manifest egress
// hosts never appear here; those are tool-plane grants served by fetch.
//
// The proxy deliberately does not inspect TLS (RUNNER-DEVIATIONS DR-6):
// the control is which host:port a tunnel may reach, not what crosses it.
// A harness that ignores HTTPS_PROXY fails closed — the internal network
// has no other route out — which is the acceptable failure direction.
//
// Part of the embedded runner-image build context (§R7): stdlib-only,
// enforced by a test in the parent package.
package netproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Proxy is an allowlisting CONNECT proxy. The zero value denies
// everything, which is the right default for a security device.
type Proxy struct {
	// Allow lists permitted CONNECT targets as host[:port]. An entry
	// without a port means 443 — model endpoints are TLS, so that is the
	// only sensible shorthand. Hosts compare case-insensitively and
	// exactly; no wildcards, because a wildcard here would quietly widen
	// the one hole in the agent's network isolation.
	Allow []string
	// Logw receives one line per CONNECT decision; nil discards. The
	// engine points this at stderr, which lands in the run's receipts.
	Logw io.Writer

	logMu sync.Mutex
}

// ListenAndServe runs the proxy on addr until the listener fails (the
// container's lifetime — the engine tears the proxy down with the run).
func (p *Proxy) ListenAndServe(addr string) error {
	p.logf("llm-proxy listening on %s (allow: %s)", addr, strings.Join(p.Allow, ", "))
	srv := &http.Server{Addr: addr, Handler: p.Handler()}
	return srv.ListenAndServe()
}

// Handler exposes the proxy as an http.Handler so tests can drive it on
// a listener they control.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(p.serveHTTP)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// No plaintext proxying: model endpoints are TLS, and CONNECT is
		// the only mode whose target this proxy can allowlist honestly.
		http.Error(w, "method not allowed: this proxy accepts CONNECT tunnels only", http.StatusMethodNotAllowed)
		return
	}
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		http.Error(w, fmt.Sprintf("malformed CONNECT target %q (want host:port)", target), http.StatusBadRequest)
		return
	}
	if !p.permits(host, port) {
		p.logf("deny %s:%s", host, port)
		http.Error(w, fmt.Sprintf("forbidden: %s:%s is not in the egress allowlist", host, port), http.StatusForbidden)
		return
	}
	p.logf("allow %s:%s", host, port)
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 30*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad gateway: dial %s: %v", target, err), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "cannot hijack client connection", http.StatusInternalServerError)
		return
	}
	client, bufrw, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		p.logf("hijack failed: %v", err)
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	// Pure byte relay from here: no buffering beyond io.Copy, and the
	// client side reads through bufrw because the hijacked buffer may
	// already hold bytes the client pipelined behind its CONNECT. When
	// either half closes, both sides are torn down — a half-open tunnel
	// serves no one and leaks two sockets.
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, bufrw)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = upstream.Close()
}

// permits applies the allowlist to one host:port pair.
func (p *Proxy) permits(host, port string) bool {
	for _, entry := range p.Allow {
		eh, ep := splitAllowEntry(entry)
		if strings.EqualFold(eh, host) && ep == port {
			return true
		}
	}
	return false
}

// splitAllowEntry parses one allowlist entry, defaulting the port to 443.
func splitAllowEntry(entry string) (host, port string) {
	entry = strings.TrimSpace(entry)
	if h, p, err := net.SplitHostPort(entry); err == nil {
		return h, p
	}
	return entry, "443"
}

// logf serializes decision lines: handlers run concurrently and receipts
// are only useful when each line is whole.
func (p *Proxy) logf(format string, args ...any) {
	if p.Logw == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	_, _ = fmt.Fprintf(p.Logw, format+"\n", args...)
}

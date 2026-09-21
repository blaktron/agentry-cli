// Package harnessprobe describes the host-run harness behind an adapter
// name on this machine: whether its executable resolves (from the operator
// policy's harness.<name>.path or PATH), which release it is, and whether
// the endpoint the policy points it at is one the run's proxy would let
// through. It reads, and executes nothing but the harness's own version
// flag; it writes nothing.
//
// `agentry doctor --adapter <name>` renders these facts as checks, and the
// desktop's first-run wizard reads them for its Coding agent step
// (agentry-cli#147, plan desktop-installers.md §9): one probe, two callers,
// the way the two clients already share the policy and install code. The
// wording of a warning lives here for the same reason: two renderings of
// one fact would drift.
package harnessprobe

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/procx"
)

// defaultTimeout bounds the version read when the caller's context sets
// no deadline of its own: a harness that hangs on --version is reported
// as unreadable, never waited on.
const defaultTimeout = 10 * time.Second

// waitDelay is how long, after the version process is killed, a child it
// left holding the output pipe may delay the read before it is abandoned.
const waitDelay = 2 * time.Second

// lookPath is the PATH lookup, a seam for tests.
var lookPath = exec.LookPath

// Result is what the probe found. Zero values mean "not applicable": a
// Result whose Known is false names an adapter the registry does not have
// (Reason says so); one whose Host is false is a containerised adapter,
// whose readiness is its image, not an executable.
type Result struct {
	// Adapter is the name that was asked about.
	Adapter string `json:"adapter"`
	// Known is false when no adapter of that name is registered.
	Known bool `json:"known"`
	// Host is true for a harness that runs as a process on this machine
	// (claude, codex); false for a containerised adapter (docker-agent).
	Host bool `json:"host"`
	// Executable is the program name the adapter resolves on PATH.
	Executable string `json:"executable,omitempty"`
	// DisplayName is the product's name for a person ("Claude Code").
	DisplayName string `json:"displayName,omitempty"`
	// InstallURL is the vendor's page for getting the harness, for a
	// client to show when Found is false.
	InstallURL string `json:"installUrl,omitempty"`
	// MCPCommand is the documented line that registers agentry-mcp with
	// this host, for a client to show and copy, never to run.
	MCPCommand string `json:"mcpCommand,omitempty"`
	// Path is the executable that resolved: it exists, though it may not
	// run (then VersionError says so). "" when Found is false.
	Path string `json:"path,omitempty"`
	// PathFromPolicy is true when Path came from harness.<name>.path.
	PathFromPolicy bool `json:"pathFromPolicy,omitempty"`
	// Found is true when an executable resolved.
	Found bool `json:"found"`
	// Reason says why Known or Found is false, in the run's own words.
	Reason string `json:"reason,omitempty"`
	// Version is the first line the harness printed on stdout for its
	// version flag; "" when it could not be read.
	Version string `json:"version,omitempty"`
	// VersionError is why Version could not be read: the process failed
	// to start, exited non-zero (its stderr follows), or ran past the
	// deadline.
	VersionError string `json:"versionError,omitempty"`
	// WrittenAgainst is the release this adapter was written against; a
	// different installed release is a warning, never a refusal.
	WrittenAgainst string `json:"writtenAgainst,omitempty"`
	// VersionMatches is true when Version contains WrittenAgainst, or
	// the adapter names no release.
	VersionMatches bool `json:"versionMatches"`
	// Warnings are the sentences a client shows for what is true but
	// not blocking: the version could not be read, or is not the one the
	// adapter was written against. Empty when Found is false.
	Warnings []string `json:"warnings,omitempty"`
	// Endpoint is the check of harness.<name>.baseUrl against the model
	// proxy allowlist; nil when the policy sets no base URL.
	Endpoint *Endpoint `json:"endpoint,omitempty"`
}

// Endpoint is harness.<name>.baseUrl held against model.proxyAllow.
type Endpoint struct {
	// BaseURL is the policy's value, with any userinfo redacted; a client
	// may show it.
	BaseURL string `json:"baseUrl"`
	// HostPort is what the proxy would see in the CONNECT; "" when the
	// URL did not parse.
	HostPort string `json:"hostPort,omitempty"`
	// Valid is false when BaseURL did not parse as a URL with a host.
	Valid bool `json:"valid"`
	// Allowed is true when HostPort is listed in model.proxyAllow.
	Allowed bool `json:"allowed"`
}

// Probe describes the harness behind adapter name under policy pol (nil
// means no policy: no path override, no endpoint check). The version read
// honours ctx's deadline, or defaultTimeout when it has none.
func Probe(ctx context.Context, pol *policy.Policy, name string) Result {
	r := Result{Adapter: name}
	ad, err := adapter.Get(name)
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	r.Known = true
	hh, ok := ad.(adapter.HostHarness)
	if !ok {
		return r
	}
	r.Host = true
	r.Executable = hh.Executable()
	r.DisplayName = hh.DisplayName()
	r.InstallURL = hh.InstallURL()
	r.MCPCommand = hh.MCPCommand()
	r.WrittenAgainst = hh.ProbeVersion()

	var hp policy.HarnessPolicy
	if pol != nil {
		hp = pol.Harness[name]
	}
	switch {
	case hp.Path != "":
		if _, statErr := os.Stat(hp.Path); statErr != nil {
			r.Reason = fmt.Sprintf("policy harness.%s.path %s: %v", name, hp.Path, statErr)
			return withEndpointCheck(r, pol, hp)
		}
		r.Path, r.PathFromPolicy, r.Found = hp.Path, true, true
	default:
		path, lerr := lookPath(hh.Executable())
		if lerr != nil {
			r.Reason = fmt.Sprintf("%s is not installed on this machine (not on PATH) — install it, or set harness.%s.path in the policy",
				hh.Executable(), name)
			return withEndpointCheck(r, pol, hp)
		}
		r.Path, r.Found = path, true
	}

	vctx := ctx
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		vctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(vctx, r.Path, hh.VersionArgv()...) //nolint:gosec // the operator's own harness, its version flag only
	cmd.WaitDelay = waitDelay
	procx.NoWindow(cmd)
	out, verr := cmd.Output()
	switch {
	case verr != nil:
		var exitErr *exec.ExitError
		if errors.As(verr, &exitErr) && len(exitErr.Stderr) > 0 {
			r.VersionError = fmt.Sprintf("%v: %s", verr, strings.TrimSpace(string(exitErr.Stderr)))
		} else {
			r.VersionError = verr.Error()
		}
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s: cannot read its version: %s", r.Path, r.VersionError))
	default:
		r.Version = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		r.VersionMatches = r.WrittenAgainst == "" || strings.Contains(r.Version, r.WrittenAgainst)
		if !r.VersionMatches {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s (%s); this adapter was written against %s, so flags or event shapes may differ",
				r.Path, r.Version, r.WrittenAgainst))
		}
	}
	return withEndpointCheck(r, pol, hp)
}

// withEndpointCheck adds the check of harness.<name>.baseUrl against the
// proxy allowlist, which does not need the executable: a misconfigured
// endpoint is worth knowing before the harness is installed.
func withEndpointCheck(r Result, pol *policy.Policy, hp policy.HarnessPolicy) Result {
	if pol == nil || hp.BaseURL == "" {
		return r
	}
	ep := &Endpoint{BaseURL: hp.BaseURL}
	r.Endpoint = ep
	u, perr := url.Parse(hp.BaseURL)
	if perr != nil || u.Host == "" {
		return r
	}
	ep.BaseURL = u.Redacted()
	ep.Valid = true
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	ep.HostPort = u.Hostname() + ":" + port
	for _, allowed := range pol.Model.ProxyAllow {
		if allowed == ep.HostPort {
			ep.Allowed = true
			break
		}
	}
	return r
}

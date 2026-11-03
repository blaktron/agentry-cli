package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/hostpin"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/messenger"
)

// A persistent agent's messenger account is held by the runner image's
// messenger role, which the engine starts beside the agent and keeps for as
// long as the agent runs, across its segments: the gateway starts an MCP
// server again for each list and call, which would drop a connection that
// must stay open. Each account has two containers on a network of its own,
// made internal so it has no route out:
//
//   - the relay, the runner image's allowlisting proxy, the one container
//     with a leg out, which tunnels to the kind's hosts and nothing else,
//     resolving each name itself and dialling only public addresses;
//   - the role, which holds the account's credential and its connection to
//     the service through the relay, and nothing else: no volume, no bind,
//     no route to the agent, the gateway or the tool plane.
//
// The credential reaches the role over its standard input, never an
// argument, the environment or a file, and the role talks to the
// supervisor over the same standard input and output, which this process
// holds through the engine's attach (which also reaches an engine in a VM).
// Under the policy's runtime both run sandboxed: the relay reads a
// resolv.conf of the host's name servers, since a sandbox does not reach
// the engine's embedded DNS, and the role is given the relay's address.

// MessengerParams start one messenger account's role.
type MessengerParams struct {
	// Kind is the messenger, Token the account's credential, and Peer the
	// person paired with the agent, if any.
	Kind  messenger.Kind
	Token string
	Peer  *messenger.Peer
	// Server is the account's own server, for a kind whose accounts name
	// one (a Matrix homeserver): the one host its relay reaches.
	Server string
	// Image is the runner image.
	Image  string
	Policy *policy.Policy
	// Dir is a folder of the account's own, for the relay's resolv.conf.
	Dir string
	// Log gets the role's and the relay's own errors: never the
	// credential, never a message.
	Log io.Writer
}

// MessengerLink is a running messenger role.
type MessengerLink struct {
	ready  messenger.Ready
	frames chan messenger.FromRole
	w      *messenger.Writer
	stop   func(context.Context)
	once   sync.Once
}

// Ready is the account as the service named it when the role connected.
func (l *MessengerLink) Ready() messenger.Ready { return l.ready }

// Frames are the role's lines after its ready line; closed when the role
// ends.
func (l *MessengerLink) Frames() <-chan messenger.FromRole { return l.frames }

// Send writes one line to the role.
func (l *MessengerLink) Send(f messenger.ToRole) error {
	if f.Start != nil {
		return errors.New("a messenger role is started once")
	}
	return l.w.Write(f)
}

// Close stops the role and removes its containers and networks.
func (l *MessengerLink) Close(ctx context.Context) {
	l.once.Do(func() { l.stop(ctx) })
}

// The relay's name on the account's network.
const messengerRelayAlias = "messenger-relay"

// messengerReadyWait bounds the wait for the role's ready line: the service
// must take the credential within it. A variable so a test need not wait.
var messengerReadyWait = 90 * time.Second

// lookupUpstreamsForMessenger reads this machine's name servers; a variable
// so a test can be any machine.
var lookupUpstreamsForMessenger = hostpin.Upstreams

// StartMessenger starts p's relay and role and returns once the service took
// the credential (the role's ready line). The containers carry a run's
// labels and this process's owner labels, so a sweep removes them if this
// process dies without closing the link.
func StartMessenger(ctx context.Context, d dockerx.Client, p MessengerParams) (_ *MessengerLink, err error) {
	if p.Token == "" {
		return nil, errors.New("the messenger account has no credential")
	}
	hosts, err := messenger.HostsFor(p.Kind, p.Server)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("the messenger %q declares no host", p.Kind.Name)
	}
	if p.Log == nil {
		p.Log = io.Discard
	}
	id, err := NewRunID()
	if err != nil {
		return nil, err
	}
	labels := runLabels(id)
	lim := policy.Limits{}.WithDefaults()
	var docker policy.DockerPolicy
	if p.Policy != nil {
		docker = p.Policy.Docker
		lim = docker.Limits.WithDefaults()
	}
	netName := "agentry-" + id + "-msg"
	relayName, roleName := containerName(id, "messenger-relay"), containerName(id, "messenger")
	var undo []func(context.Context)
	stop := func(c context.Context) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i](c)
		}
	}
	defer func() {
		if err != nil {
			stop(context.WithoutCancel(ctx))
		}
	}()
	if err := d.NetworkCreate(ctx, netName, true, labels); err != nil {
		return nil, fmt.Errorf("create the messenger's network: %w", err)
	}
	undo = append(undo, func(c context.Context) { _ = d.NetworkRemove(c, netName) })

	relay := readOnlyRoot(hardened(dockerx.RunSpec{
		Name: relayName, Image: p.Image, Network: netName, Alias: messengerRelayAlias, Labels: labels,
		Argv: []string{"proxy", "--allow", strings.Join(hosts, ","), "--public-only", "--addr", fmt.Sprintf(":%d", spec.ProxyPort)},
	}, lim), "/tmp")
	// Under the runtime where the host's name servers are known: its leg out
	// a network of its own joined at create, and a resolv.conf naming them.
	sandboxed := false
	if docker.Runtime != "" {
		rs := docker.Resolvers
		if len(rs) == 0 {
			rs = lookupUpstreamsForMessenger()
		}
		if len(rs) > 0 {
			sandboxed = true
			if err := os.MkdirAll(p.Dir, 0o700); err != nil {
				return nil, err
			}
			resolv := filepath.Join(p.Dir, "resolv.conf")
			if err := os.WriteFile(resolv, policy.ResolvConf(rs), 0o644); err != nil { //nolint:gosec // read by the relay's user; names name servers alone
				return nil, err
			}
			egress := "agentry-" + id + "-msgout"
			if err := d.NetworkCreate(ctx, egress, false, labels); err != nil {
				return nil, fmt.Errorf("create the messenger relay's network out: %w", err)
			}
			undo = append(undo, func(c context.Context) { _ = d.NetworkRemove(c, egress) })
			relay.ExtraNetworks = []string{egress}
			relay.Runtime = docker.Runtime
			relay.Mounts = append(relay.Mounts, dockerx.Mount{Source: resolv, Target: policy.ResolvConfTarget, ReadOnly: true, Relabel: true})
		}
	}
	if docker.Runtime != "" && !sandboxed {
		_, _ = fmt.Fprintf(p.Log, "the messenger relay runs on the engine's default runtime, not %s: neither the policy's docker.resolvers nor this machine names a name server it could read under it; the role runs under %s\n", docker.Runtime, docker.Runtime)
	}
	if err := dockerx.RunDetachedUnder(ctx, d, relay, "messenger relay"); err != nil {
		return nil, fmt.Errorf("start the messenger's relay: %w", err)
	}
	undo = append(undo, func(c context.Context) { _ = d.RemoveForce(c, relayName) })
	if !sandboxed {
		if err := d.NetworkConnect(ctx, "bridge", relayName); err != nil {
			return nil, fmt.Errorf("connect the messenger's relay: %w", err)
		}
	}

	role := readOnlyRoot(hardened(dockerx.RunSpec{
		Name: roleName, Image: p.Image, Network: netName, Labels: labels,
		Argv: []string{"messenger", "--proxy", fmt.Sprintf("http://%s:%d", messengerRelayAlias, spec.ProxyPort)},
		// Nobody's: it writes nothing but its own scratch. Its output is the
		// person's messages, which the engine keeps no log of.
		User: "65534:65534", Interactive: true, NoLog: true,
	}, lim), "/tmp:size=16m,mode=1777")
	if docker.Runtime != "" {
		role.Runtime = docker.Runtime
		// A sandbox does not reach the engine's DNS: the relay's name is
		// given as its address.
		ip, err := d.ContainerIP(ctx, relayName, netName)
		if err != nil || ip == "" {
			return nil, fmt.Errorf("the messenger relay's address on its network: %v", err)
		}
		role.ExtraHosts = []string{messengerRelayAlias + ":" + ip}
	}
	logw := p.Log
	// Its standard input is a pipe of the system's, handed to the engine's
	// client as a file: a reader that is not one is copied by a goroutine
	// the client's wait waits on, so a role that ended by itself (a refused
	// credential) would never be seen to.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW := io.Pipe()
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, rerr := dockerx.RunForegroundUnder(rctx, d, role, "messenger role", inR, outW, logw)
		if rerr != nil && rctx.Err() == nil {
			_, _ = fmt.Fprintf(logw, "the messenger role ended: %v\n", scrubToken(rerr.Error(), p.Token))
		}
		_ = outW.Close()
		_ = inR.Close()
	}()
	undo = append(undo, func(c context.Context) {
		// Its standard input closed, the role ends; it is removed either way.
		_ = inW.Close()
		_ = d.Stop(c, roleName, 5)
		_ = d.RemoveForce(c, roleName)
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	})
	w := messenger.NewWriter(inW)
	if err := w.Write(messenger.ToRole{Start: &messenger.Start{Kind: p.Kind.Name, Token: p.Token, Server: p.Server, Peer: p.Peer}}); err != nil {
		return nil, fmt.Errorf("the messenger role did not take its start: %w", err)
	}
	l := &MessengerLink{frames: make(chan messenger.FromRole, 64), w: w}
	ready := make(chan error, 1)
	go l.read(outR, ready)
	select {
	case err := <-ready:
		if err != nil {
			return nil, err
		}
	case <-time.After(messengerReadyWait):
		return nil, fmt.Errorf("%s did not answer the messenger role within %s", p.Kind.Title, messengerReadyWait)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	l.stop = stop
	return l, nil
}

// read takes the role's lines: its state until its ready line, which ready
// is told of, then every frame for Frames.
func (l *MessengerLink) read(r io.Reader, ready chan<- error) {
	defer close(l.frames)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), messenger.MaxLine)
	isReady := false
	why := ""
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		f, err := messenger.ReadFromRole(line)
		if err != nil {
			continue
		}
		if !isReady {
			switch {
			case f.Ready != nil:
				l.ready, isReady = *f.Ready, true
				ready <- nil
			case f.State != nil && f.State.Error != "":
				why = f.State.Error
			}
			continue
		}
		if f.Ready != nil {
			continue
		}
		l.frames <- f
	}
	if !isReady {
		if why == "" {
			why = "it ended before it connected"
		}
		ready <- errors.New("the messenger role did not connect: " + why)
	}
}

package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// The desktop link's transport is one shape on every engine and under every
// runtime: a relay of the run's own serves the link's socket in a volume the
// link entries share, and carries each call over its stdio, which this
// process holds through the engine's attach, to Agentry Desktop. No socket
// of this computer is bound into a container, so a daemon in a VM and a
// sandboxed runtime that opens no host socket carry it alike. Only a tool
// call crosses: the relay refuses any other line at its socket, and the
// bridge here drops any frame that is not a call of a bound server carrying
// the run token.

// DesktopLinkRelayLog is the receipt the relay's own errors go to.
const DesktopLinkRelayLog = "desktop-relay.log"

// desktopBridgeCalls bounds the calls the bridge carries at once: the relay
// and Agentry Desktop take as many.
const desktopBridgeCalls = 4

// startDesktopLinkRelay starts the run's desktop link relay, where the run
// has one (prepareDesktop), hands it the binding, and starts the bridge to
// the link service. It returns once the relay serves its socket, before the
// gateway starts the link entries.
func startDesktopLinkRelay(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID string) error {
	r := rd.desktop
	if r == nil {
		return nil
	}
	if err := desktopRuntimeCheck(p, rd.engine); err != nil {
		return err
	}
	if err := d.VolumeCreate(ctx, r.volume, runLabels(runID)); err != nil {
		return Classify(ExitGateway, fmt.Errorf("create the desktop link's relay volume: %w", err))
	}
	// Queued first, so it runs last: after the container that holds it.
	td.add(func(c context.Context) { _ = d.VolumeRemove(c, r.volume) })
	name := containerName(runID, desktoplink.RelayRole)
	argv := []string{desktoplink.RelayRole}
	user := rd.personUser()
	// The socket and the binding are the link's user's: given to the
	// person's ids on a rootful engine; on a rootless one, and where the
	// engine reports no ids, every container runs as the relay's root
	// already.
	chown := strings.Contains(user, ":")
	if chown {
		argv = append(argv, "--owner", user)
	}
	lim := p.limits()
	rs := dockerx.RunSpec{
		Name:  name,
		Image: p.Plan.Proxy.Image,
		Argv:  argv,
		// Root owns the fresh volume's folder, where the socket is made;
		// every capability dropped but CHOWN, to give it to the link's
		// user, with a read-only root and no network.
		User:        "0",
		Network:     "none",
		Labels:      runLabels(runID),
		Mounts:      []dockerx.Mount{{Source: r.volume, Target: desktoplink.SocketDir}},
		Interactive: true,
	}.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
	rs.ReadOnly = true
	if chown {
		rs.CapAdd = []string{"CHOWN"}
	}
	// On the daemon's default runtime, never the policy's: the relay makes
	// the socket in the volume, and a socket a gVisor sandbox makes on a
	// host-backed folder stays inside that sandbox, where no other
	// container finds it. The entries that open it run under the policy's
	// gatewayRuntime, the registration that opens host sockets.
	logf, err := os.OpenFile(filepath.Join(rd.receiptsDir, DesktopLinkRelayLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // a receipt; no secret in it
	if err != nil {
		return Classify(ExitCLI, err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, err := d.RunForeground(rctx, rs, inR, outW, logf)
		if err != nil && rctx.Err() == nil {
			_, _ = fmt.Fprintf(logf, "the relay ended: %v\n", err)
		}
		_ = outW.CloseWithError(errors.New("the desktop link's relay ended"))
		_ = inR.Close()
	}()
	td.add(func(c context.Context) {
		// Its stdin closed, the relay stops; the container is removed
		// either way, then the attach.
		_ = inW.Close()
		_ = d.Stop(c, name, 5)
		_ = d.RemoveForce(c, name)
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
		_ = logf.Close()
	})
	b := &desktopBridge{run: r, runID: runID, rd: rd, out: inW, log: logf}
	// The binding first: the relay writes it beside its socket for the link
	// entries, then listens and says it is ready.
	raw, err := json.Marshal(desktoplink.Frame{Bind: &desktoplink.Bind{Token: r.link.Token, Servers: r.bind().Servers}})
	if err != nil {
		return Classify(ExitCLI, err)
	}
	if _, err := inW.Write(append(raw, '\n')); err != nil {
		return Classify(ExitGateway, fmt.Errorf("the desktop link's relay did not take the binding: %w", err))
	}
	if err := b.start(rctx, outR, relayReadyWait); err != nil {
		logs, _ := d.Logs(ctx, name, 20)
		return Classify(ExitGateway, fmt.Errorf("the desktop link's relay did not start: %w%s", err, relayLogTail(logs)))
	}
	return nil
}

// desktopRuntimeCheck refuses a run whose policy's runtime would leave the
// link entries unable to open the relay's socket: the socket lives in a
// host-backed volume, which gVisor opens only under a registration with
// --host-uds=open, the policy's gatewayRuntime; and on podman every
// container the gateway launches takes docker.runtime itself, since podman
// drops the runtime a create names.
func desktopRuntimeCheck(p Params, eng dockerx.Engine) error {
	rt := p.Plan.Runtime()
	if rt == "" {
		return nil
	}
	if eng.Kind == dockerx.KindPodman {
		return Classify(ExitPolicy, fmt.Errorf("the desktop link's entries open a socket in the run's volume, which a sandbox under %s cannot, and on podman every container the gateway launches runs under docker.runtime: run this QuickAgent on Docker Engine, or without docker.runtime", rt))
	}
	if p.Plan.GatewayRuntime() == "" {
		return Classify(ExitPolicy, fmt.Errorf("the desktop link's entries open a socket in the run's volume, which a sandbox under %s cannot: set docker.gatewayRuntime (gVisor registered with --host-uds=open) beside docker.runtime, as the gateway needs for the run's docker filter", rt))
	}
	return nil
}

// desktopBridge carries the relay's calls to the link service.
type desktopBridge struct {
	run   *desktopRun
	runID string
	rd    *runDirs
	mu    sync.Mutex
	out   io.Writer
	log   io.Writer
	// now is the clock, a variable so a test's lines are fixed.
	now func() time.Time
}

// start waits for the relay's ready line, then serves its calls until its
// output ends.
func (b *desktopBridge) start(ctx context.Context, in io.Reader, wait time.Duration) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), desktoplink.MaxLine)
	ready := make(chan error, 1)
	go func() {
		if !sc.Scan() {
			err := sc.Err()
			if err == nil {
				err = io.EOF
			}
			ready <- err
			return
		}
		if f, err := desktoplink.ReadFrame(sc.Bytes()); err != nil || !f.Ready {
			ready <- errors.New("its first line was not its ready line")
			return
		}
		ready <- nil
		b.serve(ctx, sc)
	}()
	select {
	case err := <-ready:
		return err
	case <-time.After(wait):
		return errors.New("no ready line")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *desktopBridge) serve(ctx context.Context, sc *bufio.Scanner) {
	slots := make(chan struct{}, desktopBridgeCalls)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		f, err := desktoplink.ReadFrame(line)
		if err != nil || !f.IsCall() {
			_, _ = fmt.Fprintln(b.log, "a line from the relay that is not a tool call was dropped")
			continue
		}
		srv, bound := b.run.server(f.Server)
		tool, allowed := srv.Tool(f.Request.Tool)
		switch {
		case f.Token != b.run.link.Token:
			_, _ = fmt.Fprintln(b.log, "a call from the relay with another token than the run's was dropped")
			continue
		case !bound:
			_, _ = fmt.Fprintf(b.log, "a call from the relay for %s, which the run did not bind, was dropped\n", f.Server)
			continue
		case !allowed:
			_, _ = fmt.Fprintf(b.log, "a call from the relay for %s's %s, which the catalogue does not give the run, was dropped\n", f.Server, f.Request.Tool)
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func(id uint64, server string, tool string, class string, args json.RawMessage) {
			defer wg.Done()
			defer func() { <-slots }()
			b.answer(id, b.call(ctx, server, tool, class, args))
		}(f.ID, f.Server, f.Request.Tool, tool.Class, f.Request.Arguments)
	}
}

// call sends one call to the link service, logs it and counts it.
func (b *desktopBridge) call(ctx context.Context, server, tool, class string, args json.RawMessage) desktoplink.Answer {
	now := time.Now
	if b.now != nil {
		now = b.now
	}
	start := now()
	a, err := b.run.link.Call(ctx, desktop.CallRequest{Run: b.runID, Token: b.run.link.Token, Server: server, Tool: tool, Arguments: args})
	outcome := "ok"
	var r *desktop.Refusal
	switch {
	case errors.As(err, &r):
		outcome = "refused:" + refusalDetail(r)
		msg := r.Message
		if msg == "" {
			msg = "Agentry Desktop refused the call (" + r.Reason + ")"
		}
		a = desktoplink.TextAnswer(msg, true)
	case err != nil:
		outcome = "error"
		a = desktoplink.TextAnswer("Agentry Desktop did not answer: "+err.Error(), true)
	case a.IsError:
		outcome = "error"
	}
	b.run.count(server, class, outcome)
	b.logCall(server, desktopLogLine{
		Time: start.UTC().Format(time.RFC3339), Tool: tool, Class: class,
		Arguments: b.loggedArgs(args), Outcome: outcome, DurationMS: now().Sub(start).Milliseconds(),
	})
	return a
}

// refusalDetail is a refusal's reason as the receipt's closed list spells
// it; one outside the list reads as not installed.
func refusalDetail(r *desktop.Refusal) string {
	for _, d := range receipt.DesktopDetails() {
		if r.Reason == d {
			return d
		}
	}
	return receipt.DesktopNotInstalled
}

// loggedArgs are a call's arguments as the receipt keeps them: masked by
// the run's redactor and cut at a bound.
func (b *desktopBridge) loggedArgs(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	s := string(args)
	if b.rd != nil && b.rd.redact != nil {
		s = b.rd.redact.String(s)
	}
	if len(s) > maxDesktopLogArgs {
		cut, _ := json.Marshal(s[:maxDesktopLogArgs] + "… (cut)")
		return cut
	}
	if !json.Valid([]byte(s)) {
		quoted, _ := json.Marshal(s)
		return quoted
	}
	return json.RawMessage(s)
}

// logCall appends one line to the server's receipt.
func (b *desktopBridge) logCall(server string, line desktopLogLine) {
	if b.rd == nil || b.rd.receiptsDir == "" {
		return
	}
	raw, err := json.Marshal(line)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(b.rd.receiptsDir, desktopLogFile(server)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // a receipt, masked
	if err != nil {
		return
	}
	_, _ = f.Write(append(raw, '\n'))
	_ = f.Close()
}

func (b *desktopBridge) answer(id uint64, a desktoplink.Answer) {
	raw, err := json.Marshal(desktoplink.Frame{ID: id, Answer: &a})
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.out.Write(append(raw, '\n'))
}

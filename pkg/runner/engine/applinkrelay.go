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

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
	"github.com/blaktron/agentry-cli/runnerimage/applink"
)

// Where the engine's daemon runs in a VM (Docker Desktop, Colima, a podman
// machine), a socket on this computer cannot be bound into the run's
// containers, so the app link's container cannot bind the supervisor's. A
// relay of the run's own then serves the app link's socket in a volume the
// two containers share, and carries each call over its stdio, which this
// process holds through the engine's attach (the engine's own socket, which
// does reach the VM). The bridge here takes each call it carries to the
// supervisor's app link socket, never its owner socket, and writes the
// answer back. The relay has no network, as the app link has none, and only
// a tool call crosses: the relay refuses any other line at its socket, and
// the bridge drops any frame that is not a call.

// AppLinkRelayLog is the receipt the relay's own errors go to.
const AppLinkRelayLog = "applink-relay.log"

// appLinkVolume is the volume the relay serves the app link's socket in.
func appLinkVolume(runID string) string { return "agentry-" + runID + "-applink" }

// appLinkBridgeCalls bounds the calls the bridge carries at once: the
// relay and the supervisor take as many.
const appLinkBridgeCalls = 4

// relayReadyWait bounds the wait for the relay's ready line; a variable so
// a test need not wait.
var relayReadyWait = 60 * time.Second

// startAppLinkRelay starts the run's app link relay, where the run has one
// (prepareAppLink), and the bridge to the supervisor's socket. It returns
// once the relay serves its socket, before the gateway starts the app link.
func startAppLinkRelay(ctx context.Context, d dockerx.Client, p Params, rd *runDirs, td *teardown, runID string) error {
	if rd.appLinkRelay == "" {
		return nil
	}
	vol := rd.appLinkRelay
	if err := d.VolumeCreate(ctx, vol, runLabels(runID)); err != nil {
		return Classify(ExitGateway, fmt.Errorf("create the app link's relay volume: %w", err))
	}
	// Queued first, so it runs last: after the container that holds it.
	td.add(func(c context.Context) { _ = d.VolumeRemove(c, vol) })
	name := containerName(runID, "applink-relay")
	argv := []string{"applink-relay"}
	user := rd.personUser()
	// The socket is the app link's user's: given to the person's ids on a
	// rootful engine; on a rootless one, and where the engine reports no
	// ids, both containers run as the relay's root already.
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
		// every capability dropped but CHOWN, to give it to the app link's
		// user, with a read-only root and no network.
		User:        "0",
		Network:     "none",
		Labels:      runLabels(runID),
		Mounts:      []dockerx.Mount{{Source: vol, Target: applink.SocketDir}},
		Interactive: true,
	}.WithPosture(lim.Memory, lim.CPUs, lim.Pids)
	rs.ReadOnly = true
	if chown {
		rs.CapAdd = []string{"CHOWN"}
	}
	logf, err := os.OpenFile(filepath.Join(rd.receiptsDir, AppLinkRelayLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // a receipt; no secret in it
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
		_ = outW.CloseWithError(errors.New("the app link's relay ended"))
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
	b := &appLinkBridge{socket: rd.appLink, out: inW, log: logf}
	if err := b.start(rctx, outR, relayReadyWait); err != nil {
		logs, _ := d.Logs(ctx, name, 20)
		return Classify(ExitGateway, fmt.Errorf("the app link's relay did not start: %w%s", err, relayLogTail(logs)))
	}
	return nil
}

func relayLogTail(logs string) string {
	if logs = strings.TrimSpace(logs); logs != "" {
		return ": " + logs
	}
	return ""
}

// appLinkBridge carries the relay's calls to the supervisor's socket.
type appLinkBridge struct {
	socket string
	mu     sync.Mutex
	out    io.Writer
	log    io.Writer
}

// start waits for the relay's ready line, then serves its calls until its
// output ends.
func (b *appLinkBridge) start(ctx context.Context, in io.Reader, wait time.Duration) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), applink.MaxLine)
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
		if f, err := applink.ReadFrame(sc.Bytes()); err != nil || !f.Ready {
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

func (b *appLinkBridge) serve(ctx context.Context, sc *bufio.Scanner) {
	slots := make(chan struct{}, appLinkBridgeCalls)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		f, err := applink.ReadFrame(line)
		if err != nil || f.ID == 0 || f.Request == nil || f.Request.Tool == "" || f.Answer != nil || f.Ready {
			_, _ = fmt.Fprintln(b.log, "a line from the relay that is not a tool call was dropped")
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func(id uint64, req applink.Request) {
			defer wg.Done()
			defer func() { <-slots }()
			a, err := applink.Call(ctx, b.socket, req)
			if err != nil {
				a = applink.Answer{Text: "Agentry did not answer: " + err.Error(), IsError: true}
			}
			b.answer(id, a)
		}(f.ID, applink.Request{Tool: f.Request.Tool, Arguments: f.Request.Arguments})
	}
}

func (b *appLinkBridge) answer(id uint64, a applink.Answer) {
	raw, err := json.Marshal(applink.Frame{ID: id, Answer: &a})
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.out.Write(append(raw, '\n'))
}

// appLinkTransportLine is the receipt's line for how the app link reaches
// the supervisor.
func appLinkTransportLine(rd *runDirs) string {
	if rd.appLinkRelay == "" {
		return "the supervisor's socket " + rd.appLink + ", bound into the app link's container by its exact path"
	}
	return "a relay of the run's own (the runner image, no network) serves the app link's socket in the volume " + rd.appLinkRelay +
		", and this process carries each tool call from the relay's stdio, over the engine's own attach, to the supervisor's socket " + rd.appLink +
		"; any other line is refused at the relay's socket. A socket on this computer does not reach the run's containers here: " + rd.appLinkPlacement
}

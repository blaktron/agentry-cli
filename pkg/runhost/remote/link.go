package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// The desktop link of a job on a runhost, on the device. A release that
// declares applications on the person's computer is bound to this computer's
// Agentry Desktop before the job is sent, and the device keeps one more
// session open to the runhost for the job's life, the link verb: the runhost
// sends each of the job's tool calls (and, at its teardown, its unbind) as a
// link frame on it, and the device answers each on the session's input. The
// channel is a session this device opened over its run key; the runhost
// opens nothing towards the device.

// LinkForward answers one message a job's desktop link carries from the
// runhost: a tool call or the run's unbind. It never fails: what it cannot
// answer, it refuses.
type LinkForward func(ctx context.Context, m desktop.Message) desktop.Message

// DesktopUnreachableMessage is what the run is told when this computer's
// Agentry Desktop does not answer a call the runhost relayed.
const DesktopUnreachableMessage = "Agentry Desktop on the computer that started this run is not answering: start it again there, and the run's next call reaches it"

// SocketForward forwards the bound run's tool calls and its unbind to the
// link service on socket, and refuses anything else without sending it.
func SocketForward(socket, run string) LinkForward {
	c := desktop.Client{Socket: socket}
	return func(ctx context.Context, m desktop.Message) desktop.Message {
		if !desktop.RelayedCallOnly(m, run) {
			return desktop.Message{Refused: desktop.NotRelayed()}
		}
		ans, err := c.Forward(ctx, m)
		if err != nil {
			return desktop.Message{Refused: &desktop.Refusal{Reason: receipt.DesktopUnreachable, Message: DesktopUnreachableMessage}}
		}
		// Only an answer, a refusal or an unbind's ok goes back.
		return desktop.Message{Answer: ans.Answer, Refused: ans.Refused, OK: ans.OK}
	}
}

// LinkDialer reaches the runhost for a link session: the client, and what
// to do with it once the session ends.
type LinkDialer func(ctx context.Context) (*runhost.Client, func(), error)

// LinkStartWait bounds how long the device waits for the runhost to run the
// job before its first link session opens (the release and the run's files
// may be uploading meanwhile), and LinkRetry how often it asks.
var (
	LinkStartWait = 15 * time.Minute
	LinkRetry     = time.Second
)

// KeepLink keeps job id's link session open while the job runs, answering
// each message with forward. It returns nil once the job has ended (or never
// ran with a desktop link), and the runhost's refusal when it refuses this
// device the link. A lost connection is reached again after ReattachWaits;
// once they are spent, KeepLink gives up and the job's later calls are
// refused desktop-unreachable there. linked, when set, is told each time a
// session opens.
func KeepLink(ctx context.Context, dial LinkDialer, id string, forward LinkForward, linked func()) error {
	first := time.Now()
	ever := false
	misses := 0
	for {
		c, release, err := dial(ctx)
		if err == nil {
			a, lerr := linkOnce(ctx, c, id, forward, func() {
				ever, misses = true, 0
				if linked != nil {
					linked()
				}
			})
			release()
			switch {
			case ctx.Err() != nil:
				return nil
			case lerr == nil && a.Code == 0:
				return nil
			case lerr == nil && a.Code == runhost.LinkExitNoJob:
				if ever || time.Since(first) > LinkStartWait {
					return nil
				}
				select {
				case <-time.After(LinkRetry):
				case <-ctx.Done():
					return nil
				}
				continue
			case lerr == nil:
				return &Refusal{Code: a.Code, Reason: nonEmptyOr(a.Error, "the runhost refused the desktop link")}
			}
			err = lerr
		}
		if misses >= len(ReattachWaits) {
			return fmt.Errorf("the desktop link to job %s: %w", id, err)
		}
		select {
		case <-time.After(ReattachWaits[misses]):
		case <-ctx.Done():
			return nil
		}
		misses++
	}
}

// linkOnce holds one link session until the runhost ends it or the
// connection drops.
func linkOnce(ctx context.Context, c *runhost.Client, id string, forward LinkForward, linked func()) (runhost.Answer, error) {
	// A call still in flight when the session ends has no way back: it is
	// given up with the session.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	answers := make(chan runhost.LinkAnswer, 8)
	stop := make(chan struct{})
	var inflight sync.WaitGroup
	send := func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for {
			select {
			case a := <-answers:
				if err := enc.Encode(a); err != nil {
					return err
				}
			case <-stop:
				return nil
			case <-ctx.Done():
				return nil
			}
		}
	}
	each := func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameStarted:
			if f.Text != id {
				return fmt.Errorf("the runhost linked job %q, not this run's", f.Text)
			}
			linked()
		case runhost.FrameLink:
			if f.Text == "" {
				return errors.New("the runhost sent a link frame with no id")
			}
			inflight.Add(1)
			go func() {
				defer inflight.Done()
				m, err := desktop.ReadMessage(f.Result)
				var ans desktop.Message
				if err != nil {
					ans.Refused = &desktop.Refusal{Reason: "not-a-message", Message: "the runhost's link frame was not one message"}
				} else {
					ans = forward(sctx, m)
				}
				b, err := json.Marshal(ans)
				if err != nil {
					return
				}
				select {
				case answers <- runhost.LinkAnswer{ID: f.Text, Message: b}:
				case <-stop:
				}
			}()
		}
		return nil
	}
	a, err := c.Call(ctx, "link", []string{id}, send, each)
	close(stop)
	cancel()
	inflight.Wait()
	return a, err
}

// LinkDialerFrom reaches the runhost for a link session: through c first,
// which the caller keeps, then over a connection of its own for each later
// session.
func LinkDialerFrom(c *runhost.Client, rec *runhost.Record, keys *runhost.DeviceKeys) LinkDialer {
	first := c != nil
	return func(ctx context.Context) (*runhost.Client, func(), error) {
		if first {
			first = false
			return c, func() {}, nil
		}
		c2, err := runhost.DialRunhost(ctx, *rec, keys)
		if err != nil {
			return nil, nil, err
		}
		return c2, func() { _ = c2.Close() }, nil
	}
}

// BindingForward forwards the bound run's tool calls and its unbind to a
// binding this process holds (Agentry Desktop's own, bound in process), and
// refuses anything else without sending it.
func BindingForward(link *engine.DesktopLink, run string) LinkForward {
	return func(ctx context.Context, m desktop.Message) desktop.Message {
		if !desktop.RelayedCallOnly(m, run) || m.Call != nil && m.Call.Token != link.Token || m.Unbind != nil && m.Unbind.Token != link.Token {
			return desktop.Message{Refused: desktop.NotRelayed()}
		}
		if m.Unbind != nil {
			if link.Unbind != nil {
				if err := link.Unbind(ctx); err != nil {
					return desktop.Message{Refused: refusalOf(err)}
				}
			}
			return desktop.Message{OK: true}
		}
		a, err := link.Call(ctx, *m.Call)
		if err != nil {
			return desktop.Message{Refused: refusalOf(err)}
		}
		return desktop.Message{Answer: &a}
	}
}

// refusalOf is err as the link service's refusal: its own, or this
// computer's Agentry Desktop not answering.
func refusalOf(err error) *desktop.Refusal {
	var r *desktop.Refusal
	if errors.As(err, &r) {
		return r
	}
	return &desktop.Refusal{Reason: receipt.DesktopUnreachable, Message: DesktopUnreachableMessage}
}

// AgentsLinkID is the link verb's argument for a runhost's agents link:
// KeepLink with it holds the session that carries the runhost's agents'
// binds and calls to this computer.
const AgentsLinkID = "agents"

// AgentsForward answers a runhost's agents link with bind, a binder of this
// computer's Agentry Desktop: an agent's lazy bind binds here and the
// binding is kept by its run, a call goes to the binding of its run with the
// token it was given, an unbind ends it. Anything else is refused without
// reaching Agentry Desktop. close unbinds every binding still held, once
// the link is given up.
func AgentsForward(bind engine.DesktopBinder) (forward LinkForward, close func()) {
	var mu sync.Mutex
	held := map[string]*engine.DesktopLink{}
	forward = func(ctx context.Context, m desktop.Message) desktop.Message {
		switch {
		case m.Bind != nil && m.Call == nil && m.Unbind == nil && m.Bound == nil && m.Answer == nil && m.Refused == nil && !m.OK:
			req := *m.Bind
			if !req.Lazy || req.Agent == "" || req.Run == "" {
				return desktop.Message{Refused: desktop.NotRelayed()}
			}
			// A runhost names no folder of this computer's, and its word is
			// not the person's agreement: the agent's code calls always ask
			// here.
			req.Reports = ""
			req.Servers = slices.Clone(req.Servers)
			for i := range req.Servers {
				req.Servers[i].ExecAgreed = false
			}
			link, err := bind(ctx, req)
			if err != nil {
				return desktop.Message{Refused: refusalOf(err)}
			}
			mu.Lock()
			if old := held[req.Run]; old != nil && old.Unbind != nil {
				_ = old.Unbind(ctx)
			}
			held[req.Run] = link
			mu.Unlock()
			return desktop.Message{Bound: &desktop.Bound{Token: link.Token, Servers: link.Servers, Computer: link.Computer}}
		case m.Call != nil && m.Bind == nil && m.Unbind == nil:
			mu.Lock()
			link := held[m.Call.Run]
			mu.Unlock()
			if link == nil {
				return desktop.Message{Refused: &desktop.Refusal{Reason: receipt.DesktopUnavailable, Message: DesktopUnreachableMessage}}
			}
			return BindingForward(link, m.Call.Run)(ctx, m)
		case m.Unbind != nil && m.Bind == nil && m.Call == nil:
			mu.Lock()
			link := held[m.Unbind.Run]
			if link != nil && link.Token == m.Unbind.Token {
				delete(held, m.Unbind.Run)
			}
			mu.Unlock()
			if link == nil {
				return desktop.Message{OK: true}
			}
			return BindingForward(link, m.Unbind.Run)(ctx, m)
		}
		return desktop.Message{Refused: desktop.NotRelayed()}
	}
	close = func() {
		mu.Lock()
		links := held
		held = map[string]*engine.DesktopLink{}
		mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, l := range links {
			if l.Unbind != nil {
				_ = l.Unbind(ctx)
			}
		}
	}
	return forward, close
}

// Package desktop is how a run reaches Agentry Desktop's link service, the
// far end of the desktop link: the socket Agentry Desktop serves in the
// runner home while it runs, and the three messages on it. Before the
// sandbox starts the run binds, naming its release, its desktop line and the
// application servers it uses; the link service checks each installed server
// is present and its application reachable, and answers with a run token and
// each server's tools. While the run lives, each tool call the desktop link
// carries out of the sandbox is sent as a call with that token, and the link
// service answers it or refuses it by its reason. At teardown the run
// unbinds. One JSON line each way on a connection of its own, as the app
// link's socket takes a call.
//
// A refusal's reason is one of the receipt's desktop-refused details
// (receipt.DesktopUnavailable and the others), so what the link service
// says is what the receipt records.
package desktop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// SocketName is the link service's socket in the runner home's socket
// folder, beside the persistent agents' sockets.
const SocketName = "desktop.sock"

// SocketPath is the link service's socket for a runner home.
func SocketPath(home string) string { return filepath.Join(home, "s", SocketName) }

// MaxLine bounds a line either way.
const MaxLine = desktoplink.MaxLine

// BindTimeout bounds a bind: the link service may start an installed server
// and list its tools to answer.
var BindTimeout = 2 * time.Minute

// Message is one line on the socket: exactly one of its fields is set. The
// run sends Bind, Call or Unbind; the link service answers Bound, Answer,
// Refused or OK.
type Message struct {
	Bind   *BindRequest        `json:"bind,omitempty"`
	Call   *CallRequest        `json:"call,omitempty"`
	Unbind *UnbindRequest      `json:"unbind,omitempty"`
	Bound  *Bound              `json:"bound,omitempty"`
	Answer *desktoplink.Answer `json:"answer,omitempty"`
	// Refused says why the link service did not do what was asked.
	Refused *Refusal `json:"refused,omitempty"`
	OK      bool     `json:"ok,omitempty"`
}

// BindRequest asks the link service to bind a run: the run's id, its release
// and the application servers the run uses, each with the tools the release
// allows it.
type BindRequest struct {
	Run     string       `json:"run"`
	Ref     string       `json:"ref"`
	Digest  string       `json:"digest,omitempty"`
	Servers []BindServer `json:"servers"`
	// Agent names a persistent agent the binding is for, for the link
	// service's dialogs; empty for a run.
	Agent string `json:"agent,omitempty"`
	// ComputerUse is "app" or "screen" where the release declares computer
	// use, with the computer-use tools the run has: the link service binds
	// it in that scope or refuses the bind.
	ComputerUse   string   `json:"computerUse,omitempty"`
	ComputerTools []string `json:"computerTools,omitempty"`
}

// BindServer is one application server a run uses: its catalogue id and the
// tools the release allows, in the catalogue's words.
type BindServer struct {
	ID    string   `json:"id"`
	Tools []string `json:"tools"`
}

// Bound is a bind's answer: the run token every call carries, and each
// bound server's tools as the installed server listed them.
type Bound struct {
	Token   string                        `json:"token"`
	Servers map[string][]desktoplink.Tool `json:"servers"`
	// Computer is the computer use the link service bound, where the run
	// asked for it.
	Computer *ComputerBound `json:"computer,omitempty"`
}

// ComputerBound is what the link service bound of computer use, for the
// run's receipt: the scope it holds, the windows it holds it to, how large
// a screenshot is, what of the scope the computer's own system enforces,
// and whether the person keeps what the agent saw.
type ComputerBound struct {
	// Scope is "app" or "screen", as bound.
	Scope string `json:"scope"`
	// Windows names what the agent may see and use: the applications whose
	// windows app scope holds it to, or the display.
	Windows []string `json:"windows,omitempty"`
	// MaxEdge is the longest edge of a screenshot, in pixels: a larger
	// window is scaled down to it and positions are of the scaled image.
	MaxEdge int `json:"maxEdge,omitempty"`
	// Enforced says, in the link service's words, which of the scope the
	// computer's own system holds (the capture of one window, the input
	// held to it) and which only Agentry Desktop's check before each input.
	Enforced string `json:"enforced"`
	// Session is the display the link service works on, for example
	// "Linux, Wayland (KDE Plasma)".
	Session string `json:"session,omitempty"`
	// KeepScreenshots says the person's "Keep what the agent saw" is on:
	// each screenshot is saved in the run's own folder on their computer.
	KeepScreenshots bool `json:"keepScreenshots,omitempty"`
	// StopKey is the key that stops the agent's use of the computer, as the
	// person is told it.
	StopKey string `json:"stopKey,omitempty"`
}

// CallRequest is one tool call for a bound server.
type CallRequest struct {
	Run       string          `json:"run"`
	Token     string          `json:"token"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// UnbindRequest ends a run's binding.
type UnbindRequest struct {
	Run   string `json:"run"`
	Token string `json:"token"`
}

// Refusal says why the link service refused a bind or a call: Reason is one
// of the receipt's desktop-refused details, Server the application server it
// concerns, and Message what the person is told, in the link service's words.
type Refusal struct {
	Reason  string `json:"reason"`
	Server  string `json:"server,omitempty"`
	Message string `json:"message,omitempty"`
}

func (r *Refusal) Error() string {
	if r.Message != "" {
		return r.Message
	}
	if r.Server != "" {
		return r.Reason + ": " + r.Server
	}
	return r.Reason
}

// ReadMessage decodes one message strictly: a field it does not know
// refuses it.
func ReadMessage(line []byte) (Message, error) {
	var m Message
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Client speaks to a link service on its socket.
type Client struct {
	Socket string
}

// ErrNoService says nothing answers on the socket: Agentry Desktop is not
// running on this computer.
var ErrNoService = errors.New("no Agentry Desktop answers on this computer")

// exchange sends one message and reads one answer.
func (c Client) exchange(ctx context.Context, m Message, timeout time.Duration) (Message, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return Message{}, fmt.Errorf("%w (%v)", ErrNoService, err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Message{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Message{}, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return Message{}, err
		}
		return Message{}, errors.New("the link service closed the connection without an answer")
	}
	ans, err := ReadMessage(bytes.TrimSpace(sc.Bytes()))
	if err != nil {
		return Message{}, fmt.Errorf("the link service's answer was not one: %w", err)
	}
	return ans, nil
}

// Bind binds a run. A refusal is returned as a *Refusal.
func (c Client) Bind(ctx context.Context, req BindRequest) (Bound, error) {
	ans, err := c.exchange(ctx, Message{Bind: &req}, BindTimeout)
	if err != nil {
		return Bound{}, err
	}
	switch {
	case ans.Refused != nil:
		return Bound{}, ans.Refused
	case ans.Bound == nil || ans.Bound.Token == "":
		return Bound{}, errors.New("the link service answered the bind with neither a binding nor a refusal")
	}
	return *ans.Bound, nil
}

// Call sends one tool call. A refusal is returned as a *Refusal.
func (c Client) Call(ctx context.Context, req CallRequest) (desktoplink.Answer, error) {
	ans, err := c.exchange(ctx, Message{Call: &req}, desktoplink.CallTimeout)
	if err != nil {
		return desktoplink.Answer{}, err
	}
	switch {
	case ans.Refused != nil:
		return desktoplink.Answer{}, ans.Refused
	case ans.Answer == nil:
		return desktoplink.Answer{}, errors.New("the link service answered the call with neither an answer nor a refusal")
	}
	return *ans.Answer, nil
}

// Unbind ends the run's binding.
func (c Client) Unbind(ctx context.Context, req UnbindRequest) error {
	ans, err := c.exchange(ctx, Message{Unbind: &req}, 30*time.Second)
	if err != nil {
		return err
	}
	if ans.Refused != nil {
		return ans.Refused
	}
	return nil
}

// Handler answers a link service's messages. The link service, or a
// stand-in for it, implements it; Serve dispatches to it.
type Handler interface {
	Bind(ctx context.Context, req BindRequest) (Bound, *Refusal)
	Call(ctx context.Context, req CallRequest) (desktoplink.Answer, *Refusal)
	Unbind(ctx context.Context, req UnbindRequest) *Refusal
}

// Serve answers messages on the listener with h, one connection at a time
// per message, until ctx is done or the listener closes. A line that is not
// one message is answered with a refusal and nothing reaches h.
func Serve(ctx context.Context, ln net.Listener, h Handler) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go serveOne(ctx, c, h)
	}
}

func serveOne(ctx context.Context, c net.Conn, h Handler) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(time.Minute))
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	if !sc.Scan() {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	m, err := ReadMessage(bytes.TrimSpace(sc.Bytes()))
	var ans Message
	switch {
	case err != nil:
		ans.Refused = &Refusal{Reason: "not-a-message", Message: "the link service takes one message a connection, and this was not one"}
	case m.Bind != nil:
		b, r := h.Bind(ctx, *m.Bind)
		if r != nil {
			ans.Refused = r
		} else {
			ans.Bound = &b
		}
	case m.Call != nil:
		a, r := h.Call(ctx, *m.Call)
		if r != nil {
			ans.Refused = r
		} else {
			ans.Answer = &a
		}
	case m.Unbind != nil:
		if r := h.Unbind(ctx, *m.Unbind); r != nil {
			ans.Refused = r
		} else {
			ans.OK = true
		}
	default:
		ans.Refused = &Refusal{Reason: "not-a-message", Message: "the link service takes a bind, a call or an unbind, and this was none"}
	}
	b, err := json.Marshal(ans)
	if err != nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, _ = io.WriteString(c, string(b)+"\n")
}

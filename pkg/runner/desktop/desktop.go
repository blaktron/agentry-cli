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
	// List asks which applications the person approved on this computer,
	// and Listed answers it. A link service that does not take it answers a
	// refusal, which the client reads as ErrNoListing.
	List   *ListRequest     `json:"list,omitempty"`
	Listed *ApplicationList `json:"listed,omitempty"`
	// Refused says why the link service did not do what was asked.
	Refused *Refusal `json:"refused,omitempty"`
	OK      bool     `json:"ok,omitempty"`
}

// BindRequest asks the link service to bind a run: the run's id, its release
// and the application servers the run uses, each with the tools the release
// allows it.
type BindRequest struct {
	// Protocol is the revision of this protocol the run speaks. A bind that
	// names it is answered with From, Chosen and each tool's class; one that
	// names none is answered without them, since a run of an earlier
	// revision decodes strictly and would refuse them.
	Protocol int          `json:"protocol,omitempty"`
	Run      string       `json:"run"`
	Ref      string       `json:"ref"`
	Digest   string       `json:"digest,omitempty"`
	Servers  []BindServer `json:"servers"`
	// Agent names a persistent agent the binding is for, for the link
	// service's dialogs; empty for a run.
	Agent string `json:"agent,omitempty"`
	// Lazy binds a persistent agent's segment: an application need not be
	// running now, since whether it is is read at each call, which is
	// refused desktop-not-installed, with the step to take, while it is
	// not. The link service lists each server's tools as its installed
	// server lists them when it can start it, else as the release narrows
	// them.
	Lazy bool `json:"lazy,omitempty"`
	// ComputerUse is "app" or "screen" where the release declares computer
	// use, with the computer-use tools the run has: the link service binds
	// it in that scope or refuses the bind.
	ComputerUse   string   `json:"computerUse,omitempty"`
	ComputerTools []string `json:"computerTools,omitempty"`
	// Windows are the applications, by id, whose windows app-scope computer
	// use reaches, and ChooseWindows adds an application the person chooses
	// when the run starts.
	Windows       []string `json:"windows,omitempty"`
	ChooseWindows bool     `json:"chooseWindows,omitempty"`
	// Reports is the run's folder on this computer for what the person
	// keeps of computer use: where "Keep what the agent saw" saves each
	// screenshot. Empty where the run has no such folder.
	Reports string `json:"reports,omitempty"`
}

// ReportsFolder is the folder of a run's output, relative to it, where
// Agentry Desktop keeps the screenshots the person asked it to keep.
const ReportsFolder = "reports/desktop"

// BindServer is one application whose MCP server a run uses: its id and
// the tools the release narrows it to, none for every tool its server
// lists. For an application the person chooses when the run starts, ID is
// spec.DesktopChosenID and Chosen is set.
type BindServer struct {
	ID     string   `json:"id"`
	Tools  []string `json:"tools"`
	Chosen bool     `json:"chosen,omitempty"`
	// ExecAgreed says the person agreed that the server's tools that run
	// code run without asking each time (a persistent agent's switch); a
	// run's exec calls always ask.
	ExecAgreed bool `json:"execAgreed,omitempty"`
}

// Bound is a bind's answer: the run token every call carries, and each
// bound server's tools as the installed server listed them.
type Bound struct {
	Token   string                        `json:"token"`
	Servers map[string][]desktoplink.Tool `json:"servers"`
	// From says, per bound application id, where its set-up came from:
	// FromLibrary, FromLibraryEdited or FromPerson. Answered only to a
	// request that names Protocol.
	From map[string]string `json:"from,omitempty"`
	// Chosen is the application the person chose for the chosen entry, and
	// for windows they chose, where the run asked for one.
	Chosen *ChosenApplication `json:"chosen,omitempty"`
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

// Protocol is the revision of the link protocol this build speaks: a bind
// that names it is answered with From and Chosen.
const Protocol = 2

// Where a bound application's set-up came from (Bound.From): Agentry
// Desktop's library as it is, the library's preset the person changed, or
// the person's own.
const (
	FromLibrary       = "library"
	FromLibraryEdited = "library-edited"
	FromPerson        = "person"
)

// SetByPerson says an application's set-up is the person's rather than
// the library's: one they added, or a preset they changed. An unknown or
// missing answer reads as the person's, the strict way.
func SetByPerson(from string) bool { return from != FromLibrary }

// ChosenApplication is the application the person chose when the run
// started: its id on their computer and its name.
type ChosenApplication struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListRequest asks for the applications the person approved.
type ListRequest struct{}

// ApplicationList is the person's approved applications on this computer.
type ApplicationList struct {
	Applications []Application `json:"applications"`
}

// Application is one application the person set up in Agentry Desktop:
// what a release names it by, what it is called, where its set-up came
// from, what of it the person approved, and whether it is ready now with
// the step to take when not.
type Application struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	From     string `json:"from"`
	MCP      bool   `json:"mcp,omitempty"`
	Computer bool   `json:"computer,omitempty"`
	Ready    bool   `json:"ready,omitempty"`
	Step     string `json:"step,omitempty"`
}

// ErrNoListing says the link service answers but does not list the person's
// applications: an Agentry Desktop older than this protocol.
var ErrNoListing = errors.New("this Agentry Desktop does not list your applications: update it to list them")

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

// List asks for the person's approved applications. A link service that
// does not take the message answers ErrNoListing.
func (c Client) List(ctx context.Context) (ApplicationList, error) {
	ans, err := c.exchange(ctx, Message{List: &ListRequest{}}, 30*time.Second)
	if err != nil {
		return ApplicationList{}, err
	}
	switch {
	case ans.Refused != nil && ans.Refused.Reason == "not-a-message":
		return ApplicationList{}, ErrNoListing
	case ans.Refused != nil:
		return ApplicationList{}, ans.Refused
	case ans.Listed == nil:
		return ApplicationList{}, errors.New("the link service answered the listing with neither a list nor a refusal")
	}
	return *ans.Listed, nil
}

// Forward sends one message as it is and reads the link service's answer,
// for a relay that carries a run's calls and its unbind from another
// computer: a call waits as long as a call does, an unbind as an unbind.
func (c Client) Forward(ctx context.Context, m Message) (Message, error) {
	timeout := desktoplink.CallTimeout
	if m.Unbind != nil {
		timeout = 30 * time.Second
	}
	return c.exchange(ctx, m, timeout)
}

// Relayed is a binding made on another computer, whose link service the run
// reaches through a relay: the run id the far link service bound and its
// binding, the socket on this computer that carries each call there, and
// how the calls get there, for the receipt.
type Relayed struct {
	Socket string `json:"socket"`
	Run    string `json:"run"`
	Bound  Bound  `json:"bound"`
	Via    string `json:"via"`
}

// RelayedCallOnly says whether m is a message a relay carries for run: a
// tool call or the unbind of that run, and nothing else.
func RelayedCallOnly(m Message, run string) bool {
	switch {
	case m.Bind != nil || m.Bound != nil || m.Answer != nil || m.Refused != nil || m.OK || m.List != nil || m.Listed != nil:
		return false
	case m.Call != nil && m.Unbind == nil:
		return m.Call.Run == run && m.Call.Token != ""
	case m.Unbind != nil && m.Call == nil:
		return m.Unbind.Run == run && m.Unbind.Token != ""
	}
	return false
}

// NotRelayed is the refusal of a message a relay does not carry.
func NotRelayed() *Refusal {
	return &Refusal{Reason: "not-a-message", Message: "a relayed desktop link carries the bound run's tool calls and its unbind, and this was neither"}
}

// Handler answers a link service's messages. The link service, or a
// stand-in for it, implements it; Serve dispatches to it.
type Handler interface {
	Bind(ctx context.Context, req BindRequest) (Bound, *Refusal)
	Call(ctx context.Context, req CallRequest) (desktoplink.Answer, *Refusal)
	Unbind(ctx context.Context, req UnbindRequest) *Refusal
}

// Lister is a Handler that also lists the person's approved applications.
// A Handler that is not one answers a listing as no message it takes.
type Lister interface {
	List(ctx context.Context) (ApplicationList, *Refusal)
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
	case m.List != nil:
		l, ok := h.(Lister)
		if !ok {
			ans.Refused = &Refusal{Reason: "not-a-message", Message: "this link service does not list the person's applications"}
			break
		}
		list, r := l.List(ctx)
		if r != nil {
			ans.Refused = r
		} else {
			ans.Listed = &list
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

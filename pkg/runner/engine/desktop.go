package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/desktoplink"
)

// The desktop link. The caller binds the run to Agentry Desktop before the
// sandbox starts (over the link service's socket, or in the app's own
// process) and hands the engine the binding: the run token and each bound
// server's tools. The engine refuses a hosted run that declares the line,
// holds the binding to the plan, starts the relay that serves the link's
// socket in the run's own volume before the gateway starts the link entries,
// carries each call from the relay to the link service and the answer back,
// keeps each server's calls as a receipt, and unbinds at teardown.

// DesktopLink is a run's binding to Agentry Desktop, as the caller hands it
// to the engine.
type DesktopLink struct {
	// Token is the run token the link service issued at bind; every call
	// carries it.
	Token string
	// Servers are the bound applications' tools, by id, as the link service
	// listed them from each installed server.
	Servers map[string][]desktoplink.Tool
	// From says, per bound application id, where its set-up came from
	// (desktop.FromLibrary and the others); none from a link service older
	// than desktop.Protocol, which reads as the person's.
	From map[string]string
	// Chosen is the application the person chose for the chosen entry.
	Chosen *desktop.ChosenApplication
	// Call sends one tool call to the link service. A refusal is returned
	// as a *desktop.Refusal.
	Call func(ctx context.Context, req desktop.CallRequest) (desktoplink.Answer, error)
	// Unbind ends the binding at teardown.
	Unbind func(ctx context.Context) error
	// unbindOnce says Unbind was made to run once.
	unbindOnce bool
	// FarEnd says how the link service is reached, for the receipt.
	FarEnd string
	// Computer is the computer use the link service bound, where the plan
	// declares it.
	Computer *desktop.ComputerBound
}

// BoundDesktopLink is a binding over the link service's socket, as a run
// from the CLI makes it.
func BoundDesktopLink(c desktop.Client, run string, b desktop.Bound) *DesktopLink {
	return &DesktopLink{
		Token:    b.Token,
		Servers:  b.Servers,
		From:     b.From,
		Chosen:   b.Chosen,
		Call:     c.Call,
		Unbind:   func(ctx context.Context) error { return c.Unbind(ctx, desktop.UnbindRequest{Run: run, Token: b.Token}) },
		FarEnd:   "Agentry Desktop's link socket " + c.Socket,
		Computer: b.Computer,
	}
}

// DesktopBindRequest is the bind of run for desktop line d of release ref:
// each application with the release's narrowing of its tools (none for
// every tool its server lists), and computer use in its scope, with the
// applications whose windows it reaches and the folder of outDir where the
// person's kept screenshots go (none when outDir is empty).
func DesktopBindRequest(d *spec.DesktopCap, run, ref, digest, outDir string) desktop.BindRequest {
	req := desktop.BindRequest{Protocol: desktop.Protocol, Run: run, Ref: ref, Digest: digest}
	for _, s := range d.Servers {
		var tools []string
		if s.Narrowed {
			tools = s.ToolNames()
		}
		req.Servers = append(req.Servers, desktop.BindServer{ID: s.ID, Tools: tools, Chosen: s.Chosen})
	}
	if c, ok := d.Computer(); ok {
		req.ComputerUse, req.ComputerTools = d.ComputerUse, c.ToolNames()
		req.Windows, req.ChooseWindows = append([]string(nil), d.Windows...), d.ChooseWindows
		if outDir != "" {
			req.Reports = filepath.Join(outDir, filepath.FromSlash(desktop.ReportsFolder))
		}
	}
	return req
}

// DesktopBinder binds a run or a segment to Agentry Desktop and hands back
// the binding. A refusal is returned as a *desktop.Refusal, and no link
// service as desktop.ErrNoService.
type DesktopBinder func(ctx context.Context, req desktop.BindRequest) (*DesktopLink, error)

// SocketDesktopBinder binds over the link service's socket.
func SocketDesktopBinder(socket string) DesktopBinder {
	c := desktop.Client{Socket: socket}
	return func(ctx context.Context, req desktop.BindRequest) (*DesktopLink, error) {
		b, err := c.Bind(ctx, req)
		if err != nil {
			return nil, err
		}
		return BoundDesktopLink(c, req.Run, b), nil
	}
}

// PersistentDesktopLink binds a persistent agent's segment, lazily: the
// applications need not be running. With no Agentry Desktop answering, the
// segment starts all the same with the release's narrowing for tools, and binds at its
// first call; until one answers, each call is refused desktop-unavailable.
// A binding that goes away while the segment runs (Agentry Desktop
// restarted, a runhost's link given up and taken again) is bound again at
// the next call. A refusal of the bind is the segment's refusal.
func PersistentDesktopLink(ctx context.Context, bind DesktopBinder, req desktop.BindRequest, d *spec.DesktopCap) (*DesktopLink, error) {
	req.Lazy = true
	first, err := bind(ctx, req)
	if err != nil {
		// The person's computer away (a runhost's agent with no device
		// linked) is no Agentry Desktop answering, for now.
		var r *desktop.Refusal
		switch {
		case errors.As(err, &r) && gone(r):
		case errors.As(err, &r):
			return nil, DesktopBindRefusal(d, r)
		case !errors.Is(err, desktop.ErrNoService):
			return nil, Classify(ExitCLI, fmt.Errorf("bind the agent to Agentry Desktop: %w", err))
		}
		first = nil
	}
	return rebindingDesktopLink(bind, req, d, first)
}

// gone says a refusal is of a binding that is not there: no Agentry
// Desktop answering, or the person's computer away.
func gone(r *desktop.Refusal) bool {
	return r.Reason == receipt.DesktopUnavailable || r.Reason == receipt.DesktopUnreachable
}

// rebindingDesktopLink is a segment's binding that binds again whenever the
// one it holds is gone: a token of the segment's own between its relay and
// this process, the tools of the first binding (or the release's narrowing
// when there was none), and the bound link's token on each call it sends. Once
// unbound it binds no more.
func rebindingDesktopLink(bind DesktopBinder, req desktop.BindRequest, d *spec.DesktopCap, first *DesktopLink) (*DesktopLink, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	link := &DesktopLink{Token: hex.EncodeToString(raw[:])}
	if first != nil {
		link.Servers, link.Computer, link.FarEnd = first.Servers, first.Computer, first.FarEnd+", bound again if it goes away while the agent runs"
		link.From, link.Chosen = first.From, first.Chosen
	} else {
		link.Servers = map[string][]desktoplink.Tool{}
		for _, s := range d.Servers {
			for _, t := range s.Tools {
				link.Servers[s.ID] = append(link.Servers[s.ID], desktoplink.Tool{Name: t.Name,
					Description: s.Application + " on the person's computer, through Agentry Desktop",
					InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`)})
			}
		}
		link.FarEnd = "Agentry Desktop, bound at the agent's first call (it was not answering when the segment started)"
		if d.HasComputerUse() {
			link.Computer = &desktop.ComputerBound{Scope: d.ComputerUse,
				Enforced: "not stated: Agentry Desktop was not answering when the segment started, and states what the computer enforces once it binds"}
		}
	}
	var mu, binding sync.Mutex
	bound, closed := first, false
	held := func() (*DesktopLink, bool) {
		mu.Lock()
		defer mu.Unlock()
		return bound, closed
	}
	current := func(ctx context.Context) (*DesktopLink, error) {
		if l, done := held(); l != nil || done {
			if done {
				return nil, &desktop.Refusal{Reason: receipt.DesktopStopped, Message: "the agent's use of the person's computer has ended for this segment"}
			}
			return l, nil
		}
		binding.Lock()
		defer binding.Unlock()
		if l, done := held(); l != nil || done {
			return l, nil
		}
		l, err := bind(ctx, req)
		if err != nil {
			return nil, err
		}
		// A binding made at a call is held to the policy as the first is
		// before the segment starts.
		if id, refused := personRefused(d, l); refused {
			if l.Unbind != nil {
				_ = l.Unbind(context.WithoutCancel(ctx))
			}
			return nil, &desktop.Refusal{Reason: receipt.DesktopNotGranted, Server: id,
				Message: "this application " + policy.CustomServersRefused}
		}
		mu.Lock()
		defer mu.Unlock()
		if closed {
			if l.Unbind != nil {
				_ = l.Unbind(context.WithoutCancel(ctx))
			}
			return nil, &desktop.Refusal{Reason: receipt.DesktopStopped, Message: "the agent's use of the person's computer has ended for this segment"}
		}
		bound = l
		return l, nil
	}
	drop := func(l *DesktopLink) {
		mu.Lock()
		if bound == l {
			bound = nil
		}
		mu.Unlock()
	}
	// A refusal is kept as it came: the person's computer away is
	// desktop-unreachable, Agentry Desktop not running desktop-unavailable.
	away := func(server string, err error) error {
		var r *desktop.Refusal
		if errors.As(err, &r) {
			if r.Server == "" {
				r.Server = server
			}
			return r
		}
		return &desktop.Refusal{Reason: receipt.DesktopUnavailable, Server: server,
			Message: "Agentry Desktop is not running on the person's computer: they start it, and your next call reaches it"}
	}
	link.Call = func(ctx context.Context, c desktop.CallRequest) (desktoplink.Answer, error) {
		for attempt := 0; ; attempt++ {
			l, err := current(ctx)
			if err != nil {
				return desktoplink.Answer{}, away(c.Server, err)
			}
			c.Token = l.Token
			a, err := l.Call(ctx, c)
			var r *desktop.Refusal
			if err == nil || !errors.As(err, &r) || !gone(r) || attempt > 0 {
				return a, err
			}
			// The binding went away: bind again, once.
			drop(l)
		}
	}
	link.Unbind = func(ctx context.Context) error {
		mu.Lock()
		l := bound
		bound, closed = nil, true
		mu.Unlock()
		if l == nil || l.Unbind == nil {
			return nil
		}
		return l.Unbind(ctx)
	}
	return link, nil
}

// RelayedDesktopLink is a binding made on another computer and relayed to
// this run (a run on a remote runhost, whose device bound it): each call and
// the unbind name the run the far link service bound, over the relay's
// socket here.
func RelayedDesktopLink(r desktop.Relayed) *DesktopLink {
	c := desktop.Client{Socket: r.Socket}
	return &DesktopLink{
		Token:   r.Bound.Token,
		Servers: r.Bound.Servers,
		From:    r.Bound.From,
		Chosen:  r.Bound.Chosen,
		Call: func(ctx context.Context, req desktop.CallRequest) (desktoplink.Answer, error) {
			req.Run = r.Run
			return c.Call(ctx, req)
		},
		Unbind: func(ctx context.Context) error {
			return c.Unbind(ctx, desktop.UnbindRequest{Run: r.Run, Token: r.Bound.Token})
		},
		FarEnd:   r.Via,
		Computer: r.Bound.Computer,
	}
}

// HostedDesktopRefusal is a hosted run's refusal of a release that declares
// the person's computer: exit 11, desktop-refused, hosted. The CLI refuses
// with it before anything is installed, and the engine again.
func HostedDesktopRefusal(ids []string) error {
	return Classify(ExitPolicy, receipt.WithReason(
		errors.New("a hosted run never reaches your computer: this QuickAgent uses an application on the computer it runs from through Agentry Desktop, which only a run on your own computer does"),
		receipt.ReasonDesktopRefused, receipt.DesktopHosted, ids...))
}

// NoDesktopRefusal is the refusal of a run that declares the person's
// computer where no Agentry Desktop answers: exit 11, desktop-refused,
// desktop-unavailable, naming the applications.
func NoDesktopRefusal(d *spec.DesktopCap) error {
	return Classify(ExitPolicy, receipt.WithReason(
		fmt.Errorf("this QuickAgent uses %s on your computer, which needs Agentry Desktop running on this computer: start Agentry Desktop and run it again", desktopUses(d)),
		receipt.ReasonDesktopRefused, receipt.DesktopUnavailable, d.EntryIDs()...))
}

// desktopUses names what a line uses on the person's computer: its
// applications, and computer use.
func desktopUses(d *spec.DesktopCap) string {
	uses := make([]string, 0, len(d.Servers)+1)
	for _, s := range d.Servers {
		uses = append(uses, s.ForPerson())
	}
	if d.HasComputerUse() {
		uses = append(uses, "computer use")
	}
	return joinAnd(uses)
}

// DesktopBindRefusal is the refusal of a run whose bind the link service
// refused: exit 11, desktop-refused, with the service's reason.
func DesktopBindRefusal(d *spec.DesktopCap, r *desktop.Refusal) error {
	detail := r.Reason
	if !slices.Contains(receipt.DesktopDetails(), detail) {
		detail = receipt.DesktopNotInstalled
	}
	names := d.EntryIDs()
	if r.Server != "" {
		names = []string{r.Server}
	}
	msg := r.Message
	if msg == "" {
		msg = "Agentry Desktop refused the run's use of your computer (" + r.Reason + ")"
	}
	return Classify(ExitPolicy, receipt.WithReason(errors.New(msg), receipt.ReasonDesktopRefused, detail, names...))
}

// checkDesktop refuses, before anything is made, a run whose plan declares
// the person's computer and cannot reach it as declared: a hosted run, a
// run with no binding, and a binding that does not cover each declared
// server; and a binding given to a plan that declares none.
func checkDesktop(p Params) error {
	d := p.Plan.Capabilities.Desktop
	if d == nil {
		if p.Desktop != nil {
			return Classify(ExitCLI, errors.New("the run was bound to Agentry Desktop, and this QuickAgent declares no application on your computer"))
		}
		return nil
	}
	if p.hosted() {
		return HostedDesktopRefusal(d.EntryIDs())
	}
	if p.Desktop == nil {
		return NoDesktopRefusal(d)
	}
	if p.Desktop.Token == "" || p.Desktop.Call == nil {
		return Classify(ExitCLI, errors.New("the run's binding to Agentry Desktop has no token or no way to call it"))
	}
	for _, s := range d.Servers {
		listed, ok := p.Desktop.Servers[s.ID]
		if !ok {
			return Classify(ExitPolicy, receipt.WithReason(
				fmt.Errorf("this QuickAgent uses %s on your computer, and Agentry Desktop did not bind it for the run", s.ForPerson()),
				receipt.ReasonDesktopRefused, receipt.DesktopNotInstalled, s.ID))
		}
		for _, name := range s.ToolNames() {
			if s.Narrowed && !slices.ContainsFunc(listed, func(t desktoplink.Tool) bool { return t.Name == name }) {
				return Classify(ExitPolicy, receipt.WithReason(
					fmt.Errorf("this QuickAgent uses the tool %s of %s on your computer, which the server Agentry Desktop started for it does not list", name, boundNameForPerson(p.Desktop, s)),
					receipt.ReasonDesktopRefused, receipt.DesktopNotInstalled, s.ID))
			}
		}
	}
	if id, refused := personRefused(d, p.Desktop); refused {
		s, _ := d.Server(id)
		return Classify(ExitPolicy, receipt.WithReason(
			fmt.Errorf("this QuickAgent uses %s on your computer, which %s", boundNameForPerson(p.Desktop, s), policy.CustomServersRefused),
			receipt.ReasonDesktopRefused, receipt.DesktopNotGranted, id))
	}
	switch c := p.Desktop.Computer; {
	case d.HasComputerUse() && c == nil:
		return Classify(ExitPolicy, receipt.WithReason(
			errors.New("this QuickAgent uses computer use on your computer, and Agentry Desktop did not bind it for the run: turn computer use on in Agentry Desktop's Settings, Your computer, and run it again"),
			receipt.ReasonDesktopRefused, receipt.DesktopNotGranted, spec.DesktopComputerID))
	case d.HasComputerUse() && c.Scope != d.ComputerUse:
		return Classify(ExitPolicy, receipt.WithReason(
			fmt.Errorf("this QuickAgent's computer use asks for %s scope, and Agentry Desktop bound %s scope", d.ComputerUse, c.Scope),
			receipt.ReasonDesktopRefused, receipt.DesktopNotGranted, spec.DesktopComputerID))
	case !d.HasComputerUse() && c != nil:
		return Classify(ExitCLI, errors.New("the run was bound to computer use by Agentry Desktop, and this QuickAgent declares none"))
	}
	return nil
}

// desktopRun is the run's desktop link once prepared: the binding, the
// relay volume, and each server's calls as the bridge counts them.
type desktopRun struct {
	link *DesktopLink
	// line is the plan's desktop line.
	line   *spec.DesktopCap
	volume string
	// servers are the bound servers' tools, each narrowed to the plan's
	// allowlist, in the plan's order.
	servers []desktopServer
	mu      sync.Mutex
	// calls counts each server's calls by class, refusals by reason, and
	// errors, for the receipt's line.
	calls map[string]*desktopCounts
}

type desktopServer struct {
	spec.DesktopServerCap
	tools []desktoplink.Tool
	// from is where the application's set-up came from, as the link
	// service said.
	from string
}

// personRefused names the first application of the line whose binding in
// l is set up by the person, where the line's policy refuses that: an
// application with no answer of where its set-up came from reads as the
// person's.
func personRefused(d *spec.DesktopCap, l *DesktopLink) (string, bool) {
	if d == nil || l == nil || d.PersonServers != spec.PersonServersRefused {
		return "", false
	}
	for _, s := range d.Servers {
		if desktop.SetByPerson(l.From[s.ID]) {
			return s.ID, true
		}
	}
	return "", false
}

// tool is the bound tool of this name: one the binding gives the run, with
// the class Agentry Desktop gave it.
func (s desktopServer) tool(name string) (desktoplink.Tool, bool) {
	for _, t := range s.tools {
		if t.Name == name {
			return t, true
		}
	}
	return desktoplink.Tool{}, false
}

// boundName names a bound application in the run's words: the one the
// person chose for the chosen entry, else its id.
func boundName(l *DesktopLink, s spec.DesktopServerCap) string {
	if s.Chosen && l != nil && l.Chosen != nil && l.Chosen.Name != "" {
		return l.Chosen.Name + " (the application the person chose)"
	}
	return s.Application
}

// boundNameForPerson names a bound application in a message to the person:
// the one they chose for the chosen entry, else its id.
func boundNameForPerson(l *DesktopLink, s spec.DesktopServerCap) string {
	if s.Chosen && l != nil && l.Chosen != nil && l.Chosen.Name != "" {
		return l.Chosen.Name + " (the application you chose)"
	}
	return s.ForPerson()
}

type desktopCounts struct {
	byClass  map[string]int
	refused  map[string]int
	errors   int
	answered int
}

// desktopVolume is the volume the relay serves the link's socket in.
func desktopVolume(runID string) string { return "agentry-" + runID + "-desktop" }

// prepareDesktopLink readies the run's desktop link from its binding before
// the gateway's files are rendered, since the link entries' mount names the
// relay volume and their allowlists come from it: the volume's name, and
// each application's tools as the link service listed them, narrowed to
// the release's narrowing where it has one, which the gateway's allowlist
// is then set to.
func prepareDesktopLink(p Params, rd *runDirs, runID string) error {
	d := p.Plan.Capabilities.Desktop
	if d == nil {
		return nil
	}
	if err := checkDesktop(p); err != nil {
		return err
	}
	r := &desktopRun{link: p.Desktop, line: d, volume: desktopVolume(runID), calls: map[string]*desktopCounts{}}
	for _, s := range d.Entries() {
		listed := p.Desktop.Servers[s.ID]
		if s.ID == spec.DesktopComputerID {
			// The computer-use tools are the run's own definitions, never
			// the link service's listing.
			listed = nil
			for _, t := range desktoplink.ComputerTools() {
				tool := t.Tool
				tool.Class = t.Class
				listed = append(listed, tool)
			}
		}
		var tools []desktoplink.Tool
		for _, t := range listed {
			if !s.Narrowed || slices.Contains(s.ToolNames(), t.Name) {
				tools = append(tools, t)
			}
		}
		if s.ID != spec.DesktopComputerID {
			s.Application = boundName(p.Desktop, s)
			s.Tools = nil
			for _, t := range tools {
				s.Tools = append(s.Tools, spec.DesktopTool{Name: t.Name, Class: t.Class})
			}
		}
		r.servers = append(r.servers, desktopServer{DesktopServerCap: s, tools: tools, from: p.Desktop.From[s.ID]})
		r.calls[s.ID] = &desktopCounts{byClass: map[string]int{}, refused: map[string]int{}}
	}
	rd.desktop = r
	return nil
}

// grantDesktopTools sets each desktop link entry's allowlist in the
// gateway's rendered tools.yaml to the tools its binding gives the run, the
// same set its link lists: the plan holds only the release's narrowing,
// since which tools an application's server lists is known when the run
// binds it.
func grantDesktopTools(gatewayDir string, r *desktopRun) error {
	if r == nil {
		return nil
	}
	path := filepath.Join(gatewayDir, "tools.yaml")
	for _, s := range r.servers {
		names := make([]string, 0, len(s.tools))
		for _, t := range s.tools {
			names = append(names, t.Name)
		}
		slices.Sort(names)
		if err := setServerAllowlist(path, desktoplink.ServerName(s.ID), names); err != nil {
			return err
		}
	}
	return nil
}

// recordDesktop writes the receipt's desktop lines before any call, and has
// teardown unbind and write the final lines. The envelope is written before
// teardown runs, so the run's end refreshes the lines too (writeLines, once
// the agent has ended), whatever its outcome.
func recordDesktop(rd *runDirs, td *teardown, env *receipt.Envelope) {
	r := rd.desktop
	if r == nil {
		return
	}
	r.writeLines(env)
	env.CapabilitiesEnforced["desktop.transport"] = desktopTransportLine(r)
	td.add(func(c context.Context) {
		r.writeLines(env)
		if r.link.Unbind != nil {
			_ = r.link.Unbind(c)
		}
	})
}

// onceDesktopUnbind makes the run's binding to Agentry Desktop end once,
// however many steps ask: the teardown, and the run's or session's return.
func onceDesktopUnbind(p Params) {
	l := p.Desktop
	if l == nil || l.Unbind == nil || l.unbindOnce {
		return
	}
	var once sync.Once
	unbind := l.Unbind
	l.Unbind = func(c context.Context) error {
		var err error
		once.Do(func() { err = unbind(c) })
		return err
	}
	l.unbindOnce = true
}

// endDesktopBinding ends the run's binding to Agentry Desktop now, if it
// has one and nothing ended it yet.
func endDesktopBinding(ctx context.Context, p Params) {
	if l := p.Desktop; l != nil && l.Unbind != nil {
		_ = l.Unbind(context.WithoutCancel(ctx))
	}
}

// unbindDesktopAtTeardown has the teardown end the run's binding to Agentry
// Desktop, once, even where the run fails before recordDesktop: any later
// step of the preparation.
func unbindDesktopAtTeardown(p Params, td *teardown) {
	onceDesktopUnbind(p)
	if l := p.Desktop; l != nil && l.Unbind != nil {
		td.add(func(c context.Context) { _ = l.Unbind(c) })
	}
}

// writeLines writes each desktop line of the receipt with the calls counted
// so far. A run without the desktop link writes none.
func (r *desktopRun) writeLines(env *receipt.Envelope) {
	if r == nil {
		return
	}
	for _, s := range r.servers {
		if s.ID == spec.DesktopComputerID {
			env.CapabilitiesEnforced[desktopComputerKey] = r.computerLine(s.DesktopServerCap)
			continue
		}
		env.CapabilitiesEnforced["desktop."+s.ID] = r.receiptLine(s.DesktopServerCap)
	}
}

// bind is the binding the relay writes for the link entries: the token and
// each server's narrowed tools.
func (r *desktopRun) bind() desktoplink.Bind {
	b := desktoplink.Bind{Token: r.link.Token, Servers: map[string][]desktoplink.Tool{}}
	for _, s := range r.servers {
		b.Servers[s.ID] = s.tools
	}
	return b
}

// server is the run's server of this id, and whether the run has it.
func (r *desktopRun) server(id string) (desktopServer, bool) {
	for _, s := range r.servers {
		if s.ID == id {
			return s, true
		}
	}
	return desktopServer{}, false
}

// count records one call's outcome for the receipt.
func (r *desktopRun) count(id, class, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[id]
	if c == nil {
		return
	}
	c.byClass[class]++
	switch {
	case outcome == "ok":
		c.answered++
	case strings.HasPrefix(outcome, "refused:"):
		c.refused[strings.TrimPrefix(outcome, "refused:")]++
	default:
		c.errors++
	}
}

// callsPhrase is an entry's calls as the receipt counts them: "4 calls (2
// read, 2 act); refused: 1 outside-window; 1 not answered".
func (r *desktopRun) callsPhrase(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls[id]
	var total int
	var classes, refusals []string
	if c != nil {
		for _, class := range []string{"read", "act", "exec"} {
			if n := c.byClass[class]; n > 0 {
				total += n
				classes = append(classes, fmt.Sprintf("%d %s", n, class))
			}
		}
		reasons := make([]string, 0, len(c.refused))
		for reason := range c.refused {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			refusals = append(refusals, fmt.Sprintf("%d %s", c.refused[reason], reason))
		}
	}
	line := fmt.Sprintf("%d calls", total)
	if len(classes) > 0 {
		line += " (" + strings.Join(classes, ", ") + ")"
	}
	if len(refusals) > 0 {
		line += "; refused: " + strings.Join(refusals, ", ")
	}
	if c != nil && c.errors > 0 {
		line += fmt.Sprintf("; %d not answered", c.errors)
	}
	return line
}

// receiptLine is the receipt's line for one application server: what was
// reached, what was refused, and what the run could not enforce.
func (r *desktopRun) receiptLine(s spec.DesktopServerCap) string {
	server := "its installed server"
	from := "set up by the person in Agentry Desktop, not vetted by Agentry"
	if b, ok := r.server(s.ID); ok {
		switch b.from {
		case desktop.FromLibrary:
			from = "set up from Agentry Desktop's library"
		case desktop.FromLibraryEdited:
			from = "set up from Agentry Desktop's library and changed by the person, not vetted by Agentry as changed"
		}
		if desktop.SetByPerson(b.from) && r.line != nil && r.line.PersonServers == spec.PersonServersUnsafe {
			from += ", allowed only because Unsafe Mode is on"
		}
	}
	line := fmt.Sprintf("%s on the person's computer through %s (%s), answered through Agentry Desktop (%s); the tools allowed: %s; %s",
		s.Application, server, from, desktoplink.ServerName(s.ID), strings.Join(s.ToolNames(), ", "), r.callsPhrase(s.ID))
	return line + "; the person's computer is outside the run: its installed server ran there, outside the run's network and filesystem rules, not enforced; each call is a line of receipts/" + desktopLogFile(s.ID)
}

// desktopComputerKey is the receipt's computer-use line.
const desktopComputerKey = "desktop.computerUse"

// computerLine is the receipt's computer-use line: the scope, what the
// agent saw and at what size, what the person's computer's own system
// enforced and what only Agentry Desktop held, whether the person keeps
// what the agent saw, and the calls.
func (r *desktopRun) computerLine(s spec.DesktopServerCap) string {
	c := r.link.Computer
	if c == nil {
		c = &desktop.ComputerBound{}
	}
	scope := "app scope"
	if c.Scope == spec.ComputerUseScreen {
		scope = "screen scope, the whole display, allowed by " + map[string]string{spec.ScreenByPolicy: "the policy's desktop.allowScreen", spec.ScreenByFlag: "the run's --allow-screen", spec.ScreenByUnsafe: "Unsafe Mode"}[r.line.ScreenAllowedBy]
	}
	line := "computer use on the person's computer, answered through Agentry Desktop (" + desktoplink.ServerName(s.ID) + "), in " + scope
	if len(c.Windows) > 0 {
		line += " (" + joinAnd(c.Windows) + ")"
	}
	if c.Session != "" {
		line += ", on " + c.Session
	}
	line += "; the tools allowed: " + strings.Join(s.ToolNames(), ", ")
	if c.MaxEdge > 0 {
		line += fmt.Sprintf("; Agentry Desktop scales a screenshot to at most %d pixels on its long edge, and positions are in pixels of that image", c.MaxEdge)
	}
	enforced := c.Enforced
	if enforced == "" {
		enforced = "nothing stated"
	}
	line += "; what the computer's own system enforced, as Agentry Desktop states it: " + enforced
	line += "; the run keeps no screenshot"
	if c.KeepScreenshots {
		line += ", and Agentry Desktop states the person's \"Keep what the agent saw\" is on: it saves each screenshot in the run's folder on their computer"
	} else {
		line += ", and Agentry Desktop states the person's \"Keep what the agent saw\" is off"
	}
	if c.StopKey != "" {
		line += "; Agentry Desktop's stop key: " + c.StopKey
	}
	return line + "; " + r.callsPhrase(s.ID) + "; each action, with its window, position and typed text masked, is a line of receipts/" + desktopLogFile(s.ID)
}

// desktopTransportLine is the receipt's line for how the link's calls reach
// Agentry Desktop.
func desktopTransportLine(r *desktopRun) string {
	return "a relay of the run's own (the runner image, no network, a read-only root) serves the desktop link's socket in the volume " + r.volume +
		", which the link entries alone mount, and this process carries each tool call from the relay's stdio, over the engine's own attach, to " + r.link.FarEnd +
		" and the answer back; every frame carries the run token Agentry Desktop issued at bind, and a frame that is not a call, names a server the run did not bind or carries another token is dropped at both ends; no socket of Agentry Desktop's is bound into any container"
}

// desktopLogFile is the receipt each server's calls go to.
func desktopLogFile(id string) string { return "desktop-" + id + ".log" }

// desktopLogLine is one call as the receipt keeps it.
type desktopLogLine struct {
	Time  string `json:"time"`
	Tool  string `json:"tool"`
	Class string `json:"class"`
	// Window is the window a computer-use call saw or acted in, as Agentry
	// Desktop named it.
	Window     string          `json:"window,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	Outcome    string          `json:"outcome"`
	DurationMS int64           `json:"durationMs"`
}

// maxDesktopLogArgs bounds a call's arguments in the receipt.
const maxDesktopLogArgs = 4096

// desktopUser is spec.PHDesktopUser's value: the person's uid:gid, whose
// relay socket the link connects to.
func (rd *runDirs) desktopUser() string { return rd.personUser() }

// desktopMount is spec.PHDesktopMount's value: the relay volume at the
// socket's folder; nothing for a run with no desktop link.
func (rd *runDirs) desktopMount() string {
	if rd.desktop == nil {
		return ""
	}
	return rd.desktop.volume + ":" + desktoplink.SocketDir + ":rw"
}

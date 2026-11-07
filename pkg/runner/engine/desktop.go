package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runner/desktop"
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
	// Servers are the bound application servers' tools, by catalogue id, as
	// the link service listed them from each installed server.
	Servers map[string][]desktoplink.Tool
	// Call sends one tool call to the link service. A refusal is returned
	// as a *desktop.Refusal.
	Call func(ctx context.Context, req desktop.CallRequest) (desktoplink.Answer, error)
	// Unbind ends the binding at teardown.
	Unbind func(ctx context.Context) error
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
		Call:     c.Call,
		Unbind:   func(ctx context.Context) error { return c.Unbind(ctx, desktop.UnbindRequest{Run: run, Token: b.Token}) },
		FarEnd:   "Agentry Desktop's link socket " + c.Socket,
		Computer: b.Computer,
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
		uses = append(uses, s.Application)
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
		if _, ok := p.Desktop.Servers[s.ID]; !ok {
			return Classify(ExitPolicy, receipt.WithReason(
				fmt.Errorf("this QuickAgent uses %s on your computer, and Agentry Desktop did not bind it for the run", s.Application),
				receipt.ReasonDesktopRefused, receipt.DesktopNotInstalled, s.ID))
		}
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
// relay volume: the volume's name, and each server's tools narrowed to the
// plan's allowlist (a tool the link service listed that the catalogue does
// not give is left out, so the link lists what the gateway allows).
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
				listed = append(listed, t.Tool)
			}
		}
		var tools []desktoplink.Tool
		for _, name := range s.ToolNames() {
			for _, t := range listed {
				if t.Name == name {
					tools = append(tools, t)
					break
				}
			}
		}
		r.servers = append(r.servers, desktopServer{DesktopServerCap: s, tools: tools})
		r.calls[s.ID] = &desktopCounts{byClass: map[string]int{}, refused: map[string]int{}}
	}
	rd.desktop = r
	return nil
}

// recordDesktop writes the receipt's desktop lines before any call, and has
// teardown unbind and write the final lines.
func recordDesktop(rd *runDirs, td *teardown, env *receipt.Envelope) {
	r := rd.desktop
	if r == nil {
		return
	}
	writeLines := func() {
		for _, s := range r.servers {
			if s.ID == spec.DesktopComputerID {
				env.CapabilitiesEnforced[desktopComputerKey] = r.computerLine(s.DesktopServerCap)
				continue
			}
			env.CapabilitiesEnforced["desktop."+s.ID] = r.receiptLine(s.DesktopServerCap)
		}
	}
	writeLines()
	env.CapabilitiesEnforced["desktop.transport"] = desktopTransportLine(r)
	td.add(func(c context.Context) {
		writeLines()
		if r.link.Unbind != nil {
			_ = r.link.Unbind(c)
		}
	})
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
	line := fmt.Sprintf("%s on the person's computer through its installed server %s, answered through Agentry Desktop (%s); the tools allowed: %s; %s",
		s.Application, s.Package, desktoplink.ServerName(s.ID), strings.Join(s.ToolNames(), ", "), r.callsPhrase(s.ID))
	if len(s.Egress) > 0 {
		line += "; the server reaches " + strings.Join(s.Egress, ", ") + " from the person's computer"
	}
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

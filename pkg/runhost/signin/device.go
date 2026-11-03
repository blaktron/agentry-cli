package signin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
	"github.com/blaktron/agentry-cli/pkg/runhost"
	"github.com/blaktron/agentry-cli/pkg/runner/claudesignin"
	"github.com/blaktron/agentry-cli/pkg/runner/modelsignin"
)

// Person is the device's side of a sign-in on a runhost.
type Person struct {
	// Say tells the person what the runhost is doing.
	Say func(line string)
	// Open opens an address in the person's browser. It is handed only the
	// vendor's own sign-in pages.
	Open func(url string)
	// Code shows the code to enter on page (GitHub's device flow).
	Code func(page, code string)
	// Listen listens on the device's loopback; nil is net.Listen.
	Listen func(network, addr string) (net.Listener, error)
}

func (p Person) say(s string) {
	if p.Say != nil {
		p.Say(s)
	}
}

// Caller is the runhost connection a sign-in is made over: one Call for the
// sign-in and one for each relayed connection, all over the run key.
type Caller interface {
	Call(ctx context.Context, verb string, args []string, send func(io.Writer) error, each func(runhost.Frame) error) (runhost.Answer, error)
}

// Run signs the runhost in to vendor, with the person's browser on this
// device, and answers with what the runhost kept. app is GitHub's app for
// Copilot. Nothing of the sign-in is kept on this device.
func Run(ctx context.Context, c Caller, vendor, app string, p Person) (Kept, error) {
	if !IsVendor(vendor) {
		return Kept{}, fmt.Errorf("a runhost signs in to claude, chatgpt or copilot, not %q", vendor)
	}
	args := []string{"start", vendor}
	if app != "" {
		args = append(args, app)
	}
	page := func(addr string, o runhost.Open) bool { return vendorPage(vendor, addr, o) }
	k, err := call(ctx, c, args, nil, page, vendor == modelsignin.Claude, p)
	if err != nil {
		return Kept{}, err
	}
	if k.Vendor != vendor {
		return Kept{}, errors.New("the runhost did not say what it kept")
	}
	return k, nil
}

// call runs the sign-in verb with args, header its input's first bytes,
// and opens each address page allows; claude says the sign-in is Claude
// Code's, whose relay and narration are its own.
func call(ctx context.Context, c Caller, args []string, header []byte, page func(addr string, o runhost.Open) bool, claude bool, p Person) (Kept, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		failMu sync.Mutex
		failed error
		stops  []func()
	)
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	fail := func(err error) {
		failMu.Lock()
		if failed == nil {
			failed = err
		}
		failMu.Unlock()
		cancel()
	}
	a, err := c.Call(ctx, "sign-in", args, func(w io.Writer) error {
		if len(header) > 0 {
			if _, err := w.Write(header); err != nil {
				return err
			}
		}
		// The input stays open while the sign-in runs: its end tells the
		// runhost the device has left.
		<-ctx.Done()
		return nil
	}, func(f runhost.Frame) error {
		switch f.Type {
		case runhost.FrameOut:
			p.say(f.Text)
		case runhost.FrameOpen:
			var o runhost.Open
			if err := json.Unmarshal(f.Result, &o); err != nil {
				fail(errors.New("the runhost asked to open an address it did not describe"))
				return nil
			}
			stop, err := p.open(ctx, c, page, claude, f.Text, o)
			if err != nil {
				fail(err)
				return nil
			}
			if stop != nil {
				stops = append(stops, stop)
			}
		}
		return nil
	})
	failMu.Lock()
	ferr := failed
	failMu.Unlock()
	if ferr != nil {
		return Kept{}, ferr
	}
	if err != nil {
		return Kept{}, err
	}
	if a.Code != 0 {
		if a.Error == "" {
			a.Error = "the sign-in did not finish"
		}
		return Kept{}, fmt.Errorf("%s", a.Error)
	}
	var k Kept
	if err := json.Unmarshal(a.Result, &k); err != nil {
		return Kept{}, errors.New("the runhost did not say what it kept")
	}
	return k, nil
}

// open does what an open frame asks: shows a code and opens the page, or
// listens on the callback's port here, relays each connection to the
// runhost, and opens the address. The address must be one page allows.
func (p Person) open(ctx context.Context, c Caller, page func(string, runhost.Open) bool, claude bool, addr string, o runhost.Open) (func(), error) {
	if !page(addr, o) {
		return nil, fmt.Errorf("the runhost asked this device to open an address that is not the sign-in it started: %s", addr)
	}
	if o.Code != "" {
		if p.Code != nil {
			p.Code(addr, o.Code)
		}
		if p.Open != nil {
			p.Open(addr)
		}
		return nil, nil
	}
	stop, err := p.relay(ctx, c, o.Relay, o.Port, claude)
	if err != nil {
		return nil, fmt.Errorf("nothing kept: port %d on this computer is in use, so the browser's answer cannot reach the runhost (%v); try again", o.Port, err)
	}
	// Claude Code's sign-in on the runhost says this itself.
	if !claude {
		p.say("Approve the sign-in in your browser; it finishes on the runhost by itself.")
		p.say("If no browser opened, open this address in a browser on this computer:\n  " + addr)
	}
	if p.Open != nil {
		p.Open(addr)
	}
	return stop, nil
}

// The vendors' own pages a runhost may ask this device to open: OpenAI's
// sign-in host and GitHub's device page (variables for the tests' fakes).
var (
	chatgptHost      = hostOf(mcpauth.ChatGPTIssuer)
	githubDevicePage = mcpauth.GitHubDevicePage
)

// vendorPage says whether addr is vendor's own sign-in page as the open
// frame describes it: Anthropic's with Claude Code's callback at the
// relay's port, OpenAI's with a loopback callback at it, or GitHub's device
// page with a code.
func vendorPage(vendor, addr string, o runhost.Open) bool {
	switch vendor {
	case modelsignin.Claude:
		port, ok := claudesignin.CallbackPort(addr)
		return ok && o.Relay != "" && o.Port == port && o.Code == ""
	case modelsignin.ChatGPT:
		u, err := url.Parse(addr)
		if err != nil || u.Scheme != "https" || u.Host != chatgptHost {
			return false
		}
		port, ok := loopbackRedirect(addr, mcpauth.ChatGPTCallbackPath)
		return ok && o.Relay != "" && o.Port == port && o.Code == ""
	case modelsignin.Copilot:
		return addr == githubDevicePage && o.Code != "" && o.Relay == ""
	}
	return false
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// relay listens on this computer's loopback at port and hands each
// connection to the runhost's sign-in id over a session of its own.
func (p Person) relay(ctx context.Context, c Caller, id string, port int, ipv6 bool) (func(), error) {
	listen := p.Listen
	if listen == nil {
		listen = net.Listen
	}
	l4, err := listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	ls := []net.Listener{l4}
	// Claude Code's redirect names localhost, which a browser may resolve
	// to ::1 first; ChatGPT's names 127.0.0.1.
	if ipv6 {
		if l6, err := listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port))); err == nil {
			ls = append(ls, l6)
		}
	}
	rctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	slots := make(chan struct{}, MaxRelayed)
	for _, l := range ls {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				select {
				case slots <- struct{}{}:
				default:
					_ = conn.Close()
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-slots }()
					relayConn(rctx, c, id, conn)
				}()
			}
		}(l)
	}
	return func() {
		cancel()
		for _, l := range ls {
			_ = l.Close()
		}
		wg.Wait()
	}, nil
}

// relayConn carries one browser connection to the runhost and back.
func relayConn(ctx context.Context, c Caller, id string, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	_, _ = c.Call(ctx, "sign-in", []string{"relay", id}, func(w io.Writer) error {
		_, err := io.Copy(w, conn)
		return err
	}, func(f runhost.Frame) error {
		if f.Type == runhost.FrameData {
			if _, err := conn.Write(f.Data); err != nil {
				return err
			}
		}
		return nil
	})
}

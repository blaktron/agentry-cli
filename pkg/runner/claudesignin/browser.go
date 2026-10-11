package claudesignin

// The image's sign-in through the browser alone (agentry-cli#848; operator,
// 2026-10-11: "i dont people to have to paste a code in, i want a browser
// pop up from the container to authorize it").
//
// Claude Code's `auth login` (2.1.296) builds two addresses: it prints the
// paste-code one (redirect_uri Anthropic's own code page) and hands the
// automatic one (redirect_uri http://localhost:<port>/callback, its
// listener on 127.0.0.1:<port> in the container) to its browser opener,
// which in the image is the stand-in harnessbuild.ClaudeOpener: it writes
// "agentry-open <address>" to the container's stderr. A loopback listener in
// the container's own network namespace cannot be published, so the
// sign-in listens on the host's 127.0.0.1:<port> (and [::1]:<port>), opens
// the automatic address in the person's browser, and relays each
// connection byte for byte through `docker exec` into the container's
// loopback, as `kubectl port-forward` does. Claude Code receives the code
// on its own callback and exchanges it itself; Agentry never parses, logs
// or keeps what it relays (plan D19). Where that cannot work (no exec, the
// port taken here), the paste-code page is the fallback, for a person who
// can paste.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/dockerx"
)

// openerPrefix starts the line the stand-in opener writes.
const openerPrefix = "agentry-open "

// fallbackWait is how long, after Claude Code prints its paste-code
// address, the sign-in waits for the automatic one before it falls back.
// Claude Code hands it to the opener right after printing (2.1.296).
var fallbackWait = 10 * time.Second

// maxRelayed bounds the connections relayed at once: a browser opens one
// or two for the callback.
const maxRelayed = 8

// relayJS is the container's end of a relayed connection: a TCP pipe
// between exec's stdio and Claude Code's callback on the loopback, in the
// image's own Node. It reads and writes nothing else.
const relayJS = `const s=require("net").connect(+process.argv[1],"127.0.0.1");` +
	`process.stdin.pipe(s);s.pipe(process.stdout);` +
	`s.on("error",()=>process.exit(1));s.on("close",()=>process.exit(0))`

// callbackPort is the port of a sign-in address whose redirect is Claude
// Code's own callback, http://localhost:<port>/callback, on one of
// Anthropic's sign-in pages; false for any other address.
func callbackPort(raw string) (int, bool) {
	if !AnthropicSignInURL(raw) {
		return 0, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	r, err := url.Parse(u.Query().Get("redirect_uri"))
	if err != nil || r.Scheme != "http" || r.Hostname() != "localhost" || r.Path != "/callback" || r.User != nil || r.RawQuery != "" || r.Fragment != "" {
		return 0, false
	}
	p, err := strconv.Atoi(r.Port())
	if err != nil || p < 1024 || p > 65535 {
		return 0, false
	}
	return p, true
}

// relay listens on this computer's loopback at port and hands each
// connection to the container's own loopback at the same port through
// exec. stop closes the listeners and every connection, and waits.
func relay(ctx context.Context, ex dockerx.Execer, container string, port int) (stop func(), err error) {
	l4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	ls := []net.Listener{l4}
	// A browser may resolve localhost to ::1 first; without IPv6 here the
	// IPv4 listener is enough.
	if l6, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(port))); err == nil {
		ls = append(ls, l6)
	}
	rctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxRelayed)
	argv := []string{"node", "-e", relayJS, strconv.Itoa(port)}
	for _, l := range ls {
		wg.Add(1)
		go func(l net.Listener) {
			defer wg.Done()
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				select {
				case slots <- struct{}{}:
				default:
					_ = c.Close()
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-slots }()
					done := make(chan struct{})
					go func() {
						select {
						case <-rctx.Done():
						case <-done:
						}
						_ = c.Close()
					}()
					_, _ = ex.ExecStream(rctx, container, argv, c, c)
					close(done)
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

// browserSignIn watches Claude Code's output during the image's sign-in
// and takes the browser route, or the paste route when that cannot work.
type browserSignIn struct {
	mu  sync.Mutex
	out io.Writer
	// open opens an address in the person's browser.
	open func(string)
	// startRelay starts relaying the callback at port; nil when the daemon
	// cannot exec.
	startRelay func(port int) (func(), error)
	// canPaste: the person can paste a code at Claude Code's prompt.
	canPaste bool
	// fail ends the sign-in when neither route can work.
	fail func(error)

	manual  string
	decided bool
	// browser: the browser route was taken, so Claude Code's own paste
	// page and prompt are not shown.
	browser bool
	// held is Claude Code's output before the route is decided: shown
	// when it is, without the paste page and prompt on the browser route.
	held    []string
	streams []*stream
	timer   *time.Timer
	stops   []func()
}

// pastePrompt is Claude Code's prompt for the pasted code (2.1.296).
const pastePrompt = "Paste code here if prompted"

// stream is one of the container's two output streams, split into lines.
// Its buffer is guarded by the watcher's lock. What is not a whole line
// yet is shown as it comes once the route is decided (Claude Code's prompt
// has no newline) and kept until its line is whole, so an address split
// across writes is still found; a line that may be the opener's is held.
type stream struct {
	b     *browserSignIn
	line  []byte
	shown int
}

func (s *stream) Write(p []byte) (int, error) {
	b := s.b
	b.mu.Lock()
	defer b.mu.Unlock()
	if !slices.Contains(b.streams, s) {
		b.streams = append(b.streams, s)
	}
	s.line = append(s.line, p...)
	for {
		i := bytes.IndexByte(s.line, '\n')
		if i < 0 {
			break
		}
		b.onLine(string(s.line[:i+1]), s.shown)
		s.line, s.shown = s.line[i+1:], 0
	}
	s.flush()
	if len(s.line) > 64<<10 {
		s.line, s.shown = nil, 0
	}
	return len(p), nil
}

// flush shows the part of an unfinished line not shown yet, once the route
// is decided. Called with the watcher's lock held.
func (s *stream) flush() {
	if s.b.decided && len(s.line) > s.shown && !mayBeOpener(s.line) {
		s.b.show(string(s.line[s.shown:]))
		s.shown = len(s.line)
	}
}

// mayBeOpener reports whether a line so far is, or may become, the
// opener's.
func mayBeOpener(l []byte) bool {
	return bytes.HasPrefix(l, []byte(openerPrefix)) || bytes.HasPrefix([]byte(openerPrefix), l)
}

func (b *browserSignIn) say(text string) { _, _ = fmt.Fprintln(b.out, text) }

// show shows Claude Code's text, holds it while the route is undecided,
// and leaves out its paste page and prompt on the browser route. Called
// with mu held.
func (b *browserSignIn) show(text string) {
	switch {
	case !b.decided:
		b.held = append(b.held, text)
	case b.browser && b.pasteOnly(text):
	default:
		_, _ = io.WriteString(b.out, text)
	}
}

// pasteOnly reports whether text is Claude Code's paste page or prompt.
func (b *browserSignIn) pasteOnly(text string) bool {
	return strings.Contains(text, pastePrompt) || (b.manual != "" && strings.Contains(text, b.manual))
}

// release shows what was held, once the route is decided.
func (b *browserSignIn) release() {
	held := b.held
	b.held = nil
	for _, text := range held {
		b.show(text)
	}
	for _, s := range b.streams {
		s.flush()
	}
}

// onLine takes one complete line of Claude Code's output, of which the
// first shown bytes were already shown; called with mu held. The opener's line is the
// sign-in's own and is not shown; every other line is.
func (b *browserSignIn) onLine(line string, shown int) {
	if rest, ok := strings.CutPrefix(line, openerPrefix); ok && shown == 0 {
		b.automatic(strings.TrimSpace(rest))
		return
	}
	if !b.decided && b.manual == "" {
		b.findManual(line)
	}
	b.show(line[shown:])
}

// findManual remembers Claude Code's paste-code page, the fallback, and
// starts the wait for the browser address. Called with mu held.
func (b *browserSignIn) findManual(line string) {
	for _, m := range urlRE.FindAllString(line, -1) {
		if AnthropicSignInURL(m) {
			b.manual = m
			b.timer = time.AfterFunc(fallbackWait, func() {
				b.mu.Lock()
				defer b.mu.Unlock()
				b.fallBack("Claude Code did not hand over its browser address")
			})
			return
		}
	}
}

// automatic takes the address Claude Code handed its opener.
func (b *browserSignIn) automatic(addr string) {
	if b.decided {
		return
	}
	port, ok := callbackPort(addr)
	switch {
	case !ok:
		b.fallBack("Claude Code handed over an address that is not Anthropic's sign-in with its own callback")
		return
	case b.startRelay == nil:
		b.fallBack("this docker client cannot relay the browser's answer into the container")
		return
	}
	stop, err := b.startRelay(port)
	if err != nil {
		b.fallBack(fmt.Sprintf("port %d on this computer is in use", port))
		return
	}
	b.stops = append(b.stops, stop)
	b.decided, b.browser = true, true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.release()
	b.say("Approve the sign-in in your browser; it finishes here by itself.")
	b.say("If no browser opened, open this address in a browser on this computer:\n  " + addr)
	b.open(addr)
}

// fallBack takes the paste route, or ends the sign-in when the person
// cannot paste. Called with mu held.
func (b *browserSignIn) fallBack(why string) {
	if b.decided {
		return
	}
	b.decided = true
	b.release()
	if b.canPaste && b.manual != "" {
		b.say("The browser cannot hand the sign-in back to this computer (" + why + "). When Anthropic's page shows a code, copy it, paste it at Claude Code's prompt below and press Enter.")
		b.open(b.manual)
		return
	}
	b.fail(errors.New("nothing kept: the browser sign-in could not reach Claude Code on this computer (" + why + "); try again"))
}

// close stops the relay and the fallback timer.
func (b *browserSignIn) close() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
	}
	if !b.decided {
		b.decided = true
		b.release()
	}
	stops := b.stops
	b.stops = nil
	b.mu.Unlock()
	for _, s := range stops {
		s()
	}
}

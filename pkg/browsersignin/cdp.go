package browsersignin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// maxMessage bounds one message read from the browser.
const maxMessage = 32 << 20

// browser is a started browser and its DevTools pipe: messages are JSON,
// each ended by a NUL byte.
type browser struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	exited chan struct{}

	mu      sync.Mutex
	nextID  int
	waiting map[int]chan reply
	readErr error
}

type reply struct {
	Result json.RawMessage
	Err    error
}

// begin reads the browser's messages and notes its exit; start calls it
// once the process runs.
func (b *browser) begin(out io.Reader) {
	b.waiting = map[int]chan reply{}
	b.exited = make(chan struct{})
	go func() {
		_ = b.cmd.Wait()
		close(b.exited)
	}()
	go b.read(out)
}

func (b *browser) hasExited() bool {
	select {
	case <-b.exited:
		return true
	default:
		return false
	}
}

func (b *browser) read(out io.Reader) {
	r := bufio.NewReaderSize(out, 64<<10)
	var err error
	for {
		var msg []byte
		msg, err = readMessage(r)
		if err != nil {
			break
		}
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &m) != nil || m.ID == 0 {
			// An event: nothing here waits on one.
			continue
		}
		rep := reply{Result: m.Result}
		if m.Error != nil {
			rep.Err = errors.New(m.Error.Message)
		}
		b.mu.Lock()
		ch := b.waiting[m.ID]
		delete(b.waiting, m.ID)
		b.mu.Unlock()
		if ch != nil {
			ch <- rep
		}
	}
	b.mu.Lock()
	b.readErr = err
	for id, ch := range b.waiting {
		ch <- reply{Err: fmt.Errorf("the browser's pipe closed: %w", err)}
		delete(b.waiting, id)
	}
	b.mu.Unlock()
}

// readMessage reads up to the next NUL.
func readMessage(r *bufio.Reader) ([]byte, error) {
	var msg []byte
	for {
		chunk, err := r.ReadSlice(0)
		msg = append(msg, chunk...)
		if len(msg) > maxMessage {
			return nil, errors.New("a message from the browser is too long")
		}
		if err == nil {
			return msg[:len(msg)-1], nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

// call sends one command to the browser and waits for its answer.
func (b *browser) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan reply, 1)
	b.mu.Lock()
	if b.readErr != nil {
		err := b.readErr
		b.mu.Unlock()
		return nil, fmt.Errorf("the browser's pipe closed: %w", err)
	}
	b.nextID++
	id := b.nextID
	b.waiting[id] = ch
	b.mu.Unlock()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if _, err := b.in.Write(append(data, 0)); err != nil {
		b.forget(id)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case rep := <-ch:
		return rep.Result, rep.Err
	case <-ctx.Done():
		b.forget(id)
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

func (b *browser) forget(id int) {
	b.mu.Lock()
	delete(b.waiting, id)
	b.mu.Unlock()
}

// open shows address in the window the browser started with, by opening
// it in a new tab and closing the blank one, so the address, which may be a
// secret, never appears on the browser's command line.
func (b *browser) open(ctx context.Context, address string) error {
	blank, err := b.pages(ctx)
	if err != nil {
		return err
	}
	if _, err := b.call(ctx, "Target.createTarget", map[string]any{"url": address}); err != nil {
		return err
	}
	for _, id := range blank {
		_, _ = b.call(ctx, "Target.closeTarget", map[string]any{"targetId": id})
	}
	return nil
}

// pages are the ids of the browser's pages.
func (b *browser) pages(ctx context.Context) ([]string, error) {
	raw, err := b.call(ctx, "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		TargetInfos []struct {
			ID   string `json:"targetId"`
			Type string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	var out []string
	for _, t := range r.TargetInfos {
		if t.Type == "page" {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

// pageOpen says a window still shows a page: closing the last one ends the
// sign-in.
func (b *browser) pageOpen(ctx context.Context) (bool, error) {
	ids, err := b.pages(ctx)
	return len(ids) > 0, err
}

// cookies are every cookie the browser's profile holds.
func (b *browser) cookies(ctx context.Context) ([]Cookie, error) {
	raw, err := b.call(ctx, "Storage.getCookies", map[string]any{})
	if err != nil {
		return nil, err
	}
	var r struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.Cookies, nil
}

// close ends the browser: it is asked to close, then killed if it has not
// within a few seconds.
func (b *browser) close() {
	if !b.hasExited() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = b.call(ctx, "Browser.close", nil)
		cancel()
	}
	_ = b.in.Close()
	select {
	case <-b.exited:
	case <-time.After(5 * time.Second):
		_ = b.cmd.Process.Kill()
		<-b.exited
	}
}

// flags are the browser's command line but for the pipe's: a profile of
// its own with no first-run pages, no sync and no prompt for the system
// keychain (the profile is removed afterwards), and Google's device-bound
// sessions off, which would tie the sign-in's cookies to this browser
// alone.
func flags(profile string) []string {
	return []string{
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		"--password-store=basic",
		"--use-mock-keychain",
		"--disable-features=EnableBoundSessionCredentials",
		"--new-window",
		"about:blank",
	}
}

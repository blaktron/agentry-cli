package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blaktron/agentry-cli/runnerimage/harness/llm"
	"github.com/blaktron/agentry-cli/runnerimage/harness/proto"
)

// retried reports whether a failure of class may pass by itself: the server
// was busy or failing, or the connection broke. A request the server
// refused for what it is (too long, malformed, an unpaid account, a key it
// does not take) fails the same way however often it is sent.
func retried(class llm.Class) bool {
	switch class {
	case llm.Capacity, llm.RateLimit, llm.Transport, llm.Server:
		return true
	}
	return false
}

// stream sends one model request, and sends it again after a failure that
// may pass, as the configuration's retry policy says. Each new attempt is
// reported as a warning before its wait.
func (h *Harness) stream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Response, error) {
	for attempt := 1; ; attempt++ {
		// What a failed attempt wrote is part of no reply.
		h.said.Reset()
		reply, err := h.model.Stream(ctx, req, onDelta)
		if err == nil {
			return reply, nil
		}
		var me *llm.Error
		policy := h.cfg.Retry
		if policy == nil || attempt > policy.Attempts || !errors.As(err, &me) || !retried(me.Class) || ctx.Err() != nil {
			return nil, err
		}
		// The first wait doubled for each attempt before this one, and
		// never past the longest.
		longest := time.Duration(policy.MaxSeconds) * time.Second
		wait := time.Duration(policy.BaseSeconds) * time.Second
		for i := 1; i < attempt && wait < longest; i++ {
			wait *= 2
		}
		wait = min(wait, longest)
		if me.RetryAfter > 0 {
			if me.RetryAfter > longest {
				// Longer than the harness waits: the runner decides
				// what to do with a server that asks for this much.
				return nil, err
			}
			wait = me.RetryAfter
		}
		h.emit(proto.Event{
			Type: proto.TypeWarning, Code: proto.WarnRetry, Class: string(me.Class), Status: me.Status, Count: attempt,
			RetryAfterSeconds: int(wait.Seconds()),
			Text:              fmt.Sprintf("The model request failed (%s) and is sent again in %s, attempt %d of %d.", me.Class, wait, attempt, policy.Attempts),
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// idleTransport ends a request whose server has sent nothing for idle: no
// response headers, or no further byte of the reply. A model that streams
// for an hour is not cut off; one whose connection hangs is.
type idleTransport struct {
	base http.RoundTripper
	idle time.Duration
}

func (t *idleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	w := &idleWatch{idle: t.idle, cancel: cancel}
	w.timer = time.AfterFunc(t.idle, w.expire)
	res, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		w.stop()
		if w.expired.Load() {
			return nil, w.err()
		}
		return nil, err
	}
	res.Body = &idleBody{ReadCloser: res.Body, watch: w}
	return res, nil
}

type idleWatch struct {
	idle    time.Duration
	cancel  context.CancelFunc
	timer   *time.Timer
	expired atomic.Bool
	once    sync.Once
}

func (w *idleWatch) expire() {
	w.expired.Store(true)
	w.cancel()
}

func (w *idleWatch) stop() {
	w.once.Do(func() {
		w.timer.Stop()
		w.cancel()
	})
}

func (w *idleWatch) err() error {
	return fmt.Errorf("the model's server sent nothing for %s", w.idle)
}

// idleBody is a reply's body that restarts the watch at every read.
type idleBody struct {
	io.ReadCloser
	watch *idleWatch
}

func (b *idleBody) Read(p []byte) (int, error) {
	if !b.watch.expired.Load() {
		b.watch.timer.Reset(b.watch.idle)
	}
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && b.watch.expired.Load() {
		err = b.watch.err()
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.watch.stop()
	return b.ReadCloser.Close()
}

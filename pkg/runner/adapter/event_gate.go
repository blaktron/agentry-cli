package adapter

import "sync"

// eventGate orders a session's event sends against the close of its event
// channel. A driver sends from more than one goroutine: its reader, and
// the caller of Prompt, which reports the turn's usage after the answer
// arrives. Close may run between the two, so a send after it is dropped
// rather than made on a closed channel, which would panic and take the
// run down mid-turn.
//
// A send may block while the channel is full; the consumer drains the
// channel until it closes, so close waits at most for that send.
type eventGate struct {
	mu     sync.RWMutex
	closed bool
}

// send delivers ev on ch unless ch has been closed through the gate.
func (g *eventGate) send(ch chan<- Event, ev Event) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.closed {
		ch <- ev
	}
}

// close closes ch once; later sends through the gate are dropped.
func (g *eventGate) close(ch chan<- Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.closed = true
		close(ch)
	}
}

package engine

import (
	"sync"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// TestEmitAfterTheEventsCloseIsDropped: a turn that emits after End has
// closed the consumer's channel drops the event instead of panicking with
// a send on a closed channel. The session has ended, so done is closed
// too, and a select over both could otherwise pick the closed send.
func TestEmitAfterTheEventsCloseIsDropped(t *testing.T) {
	s := &liveSession{events: make(chan session.Event, 1), done: make(chan struct{})}
	close(s.done)
	s.closeEvents()
	for i := 0; i < 100; i++ {
		s.emit(session.Event{Kind: session.KindAssistantText, Text: "late"})
	}
	if _, open := <-s.events; open {
		t.Fatal("an event emitted after the close was delivered")
	}
}

// TestEndingWhileATurnEmits: closing the events while turns still emit,
// under -race, neither panics nor races on the channel.
func TestEndingWhileATurnEmits(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := &liveSession{events: make(chan session.Event, 2), done: make(chan struct{})}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				s.emit(session.Event{Kind: session.KindAssistantText, Text: "turn"})
			}
		}()
		go func() {
			defer wg.Done()
			close(s.done)
			s.closeEvents()
		}()
		for range s.events {
		}
		wg.Wait()
	}
}

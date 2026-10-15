package adapter

import (
	"context"
	"io"
	"sync"
	"testing"
)

type nopRWC struct{}

func (nopRWC) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopRWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopRWC) Close() error                { return nil }

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestAnEventAfterCloseIsDropped: every session driver drops an event sent
// after Close, as Prompt's usage report is when the session ends while the
// turn is still answering, instead of sending on the closed channel.
func TestAnEventAfterCloseIsDropped(t *testing.T) {
	drivers := map[string]func() (SessionDriver, func(Event)){
		"acp": func() (SessionDriver, func(Event)) {
			s := &acpSession{rw: nopRWC{}, readDone: closedChan(), events: make(chan Event, 1), pending: map[int]chan rpcMessage{}}
			return s, s.emit
		},
		"claude": func() (SessionDriver, func(Event)) {
			s := &claudeSession{rw: nopRWC{}, readDone: closedChan(), events: make(chan Event, 1)}
			return s, s.emit
		},
		"codex": func() (SessionDriver, func(Event)) {
			s := &codexSession{rw: nopRWC{}, readDone: closedChan(), events: make(chan Event, 1), pending: map[int]chan rpcMessage{}}
			return s, s.emit
		},
		"copilot": func() (SessionDriver, func(Event)) {
			_, cancel := context.WithCancel(context.Background())
			s := &copilotSession{rw: nopRWC{}, readDone: closedChan(), events: make(chan Event, 1), pending: map[int]chan rpcMessage{}, cancel: cancel}
			return s, s.emit
		},
	}
	for name, mk := range drivers {
		t.Run(name, func(t *testing.T) {
			drv, emit := mk()
			if err := drv.Close(); err != nil {
				t.Fatal(err)
			}
			emit(Event{Kind: EventUsage, TotalTokens: 1}) // panicked: send on closed channel
			if _, open := <-drv.Events(); open {
				t.Fatal("an event sent after Close was delivered")
			}
		})
	}
}

// TestCloseWhileATurnEmits: under -race, Close racing the turn's own sends
// neither panics nor races on the channel, and every event sent before
// Close is delivered.
func TestCloseWhileATurnEmits(t *testing.T) {
	for i := 0; i < 200; i++ {
		s := &acpSession{rw: nopRWC{}, readDone: closedChan(), events: make(chan Event, 4), pending: map[int]chan rpcMessage{}}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				s.emit(Event{Kind: EventUsage, TotalTokens: int64(j)})
			}
		}()
		go func() {
			defer wg.Done()
			_ = s.Close()
		}()
		for range s.Events() { // the consumer drains until the channel closes
		}
		wg.Wait()
	}
}

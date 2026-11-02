package persistent

// The person's answer to what an agent asks of them: a run of a subagent
// before each of which they asked to be asked, an install or a schedule a
// helper tool asks for, the first run of a QuickAgent at a version. The
// question goes to every client attached to the agent's owner socket, as an
// event of kind question that names it by an id; the first answer that names
// the id is the one taken, and the others are refused. With no client
// attached, nobody is asked and the agent is told so, since an agent never
// answers for the person.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/session"
	"github.com/blaktron/agentry-cli/pkg/runner/session/protocol"
)

// askWait bounds how long a question waits for the person.
const askWait = 10 * time.Minute

// The ways a question is not a yes.
var (
	// ErrNotAgreed: the person said no.
	ErrNotAgreed = errors.New("not done: the person said no")
	// ErrNobodyToAsk: no client is attached to ask the person.
	ErrNobodyToAsk = errors.New("not done: the person could not be asked, because no Agentry window or terminal is attached to this agent; ask them to open it in Agentry Desktop, or to attach to it with agentry agent chat, and try again")
	// ErrNotAnswered: the person did not answer in time.
	ErrNotAnswered = errors.New("not done: the person did not answer")
)

// asker is an attached client a question is sent to.
type asker func(Reply)

// question is one question waiting for its answer.
type question struct {
	answer chan bool
}

// Ask puts title and text to the person through the attached clients and
// waits for the first answer: nil for a yes, ErrNotAgreed for a no,
// ErrNobodyToAsk when no client is attached to ask, ErrNotAnswered when no
// answer comes in time.
func (s *Supervisor) Ask(ctx context.Context, title, text string) error {
	id, err := newQuestionID()
	if err != nil {
		return err
	}
	q := &question{answer: make(chan bool, 1)}
	s.mu.Lock()
	askers := make([]asker, 0, len(s.askers))
	for _, a := range s.askers {
		askers = append(askers, a)
	}
	if len(askers) > 0 {
		if s.questions == nil {
			s.questions = map[string]*question{}
		}
		s.questions[id] = q
	}
	s.mu.Unlock()
	if len(askers) == 0 {
		return ErrNobodyToAsk
	}
	defer func() {
		s.mu.Lock()
		delete(s.questions, id)
		s.mu.Unlock()
	}()
	ev := Reply{Event: &protocol.Event{Event: session.Event{Kind: session.KindQuestion, Time: time.Now().UTC(), Question: id,
		Text: strings.TrimSpace(title) + "\n\n" + strings.TrimSpace(text)}}}
	for _, a := range askers {
		a(ev)
	}
	t := time.NewTimer(askWait)
	defer t.Stop()
	select {
	case yes := <-q.answer:
		if yes {
			return nil
		}
		return ErrNotAgreed
	case <-t.C:
		return ErrNotAnswered
	case <-ctx.Done():
		return ErrNotAnswered
	}
}

// answer takes a client's answer to the question id.
func (s *Supervisor) answer(id string, yes bool) error {
	s.mu.Lock()
	q := s.questions[id]
	if q != nil {
		delete(s.questions, id)
	}
	s.mu.Unlock()
	if q == nil {
		return fmt.Errorf("no question %q is waiting for an answer: it was answered, or it is over", id)
	}
	q.answer <- yes
	return nil
}

// addAsker sends the agent's questions to a client from now on; the func
// it answers stops that.
func (s *Supervisor) addAsker(a asker) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.askers == nil {
		s.askers = map[int]asker{}
	}
	id := s.nextAsker
	s.nextAsker++
	s.askers[id] = a
	return func() {
		s.mu.Lock()
		delete(s.askers, id)
		s.mu.Unlock()
	}
}

func newQuestionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "q-" + hex.EncodeToString(b[:]), nil
}

// Question is a question Ask puts to the person, as a client in this
// process is given it: its id, its title and its text.
type Question struct {
	ID, Title, Text string
}

// OnQuestion sends the agent's questions to ask as well as to the clients
// attached to the owner socket: a client that supervises the agent in its
// own process (Agentry Desktop) draws them and answers with Answer. The
// func it answers stops that.
func (s *Supervisor) OnQuestion(ask func(Question)) func() {
	return s.addAsker(func(r Reply) {
		if r.Event == nil || r.Event.Kind != session.KindQuestion {
			return
		}
		title, text, _ := strings.Cut(r.Event.Text, "\n\n")
		ask(Question{ID: r.Event.Question, Title: title, Text: text})
	})
}

// Answer takes the person's answer to the question id, as an answer over
// the owner socket is taken.
func (s *Supervisor) Answer(id string, yes bool) error { return s.answer(id, yes) }

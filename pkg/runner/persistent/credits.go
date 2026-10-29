package persistent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/session"
)

// An agent whose policy pays with Agentry credits (model provider agentry)
// holds one credits session for its whole life: opened when the agent
// starts, renewed here every RenewEvery, ended when it stops. Every
// segment is handed the same token, so a segment that starts again (a
// change, a compaction) keeps it, and the harness never restarts for a
// renewal.

// paysWithCredits says the agent's policy pays with Agentry credits.
func (s *Supervisor) paysWithCredits() bool {
	return s.opts.Policy != nil && s.opts.Policy.Model.Provider == policy.ProviderAgentry
}

// openCredits opens the agent's credits session, when it pays with credits.
func (s *Supervisor) openCredits(ctx context.Context) error {
	if !s.paysWithCredits() {
		return nil
	}
	if s.opts.Credits == nil {
		return engine.Classify(engine.ExitPolicy, receipt.WithReason(errors.New("this agent pays for its model with Agentry credits (model provider agentry), and this computer is not signed in to the registry its policy names: agentry login"),
			receipt.ReasonSignInMissing, ""))
	}
	o, err := s.opts.Credits.Open(ctx, inference.PurposeAgent)
	if err != nil {
		if errors.Is(err, inference.ErrNoCredits) {
			return engine.Classify(engine.ExitPolicy, receipt.WithReason(err, receipt.ReasonOutOfCredits, ""))
		}
		return engine.Classify(engine.ExitPolicy, err)
	}
	s.mu.Lock()
	s.credits = &engine.InferenceGrant{Session: o.Session.ID, Token: o.Token, Model: o.Model, RenewEvery: o.RenewEvery}
	s.mu.Unlock()
	return nil
}

// creditsGrant is the grant a segment is handed: the session's token,
// with no Renew or End, which are the supervisor's.
func (s *Supervisor) creditsGrant() *engine.InferenceGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credits == nil {
		return nil
	}
	g := *s.credits
	return &g
}

// startCreditsRenewal renews the agent's credits session for as long as
// the agent runs. A session the registry has ended (the computer slept
// past its hour, or the sign-in was revoked) is opened again, and the
// running segment restarts on the new token, telling the watchers why.
func (s *Supervisor) startCreditsRenewal() {
	g := s.creditsGrant()
	if g == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.mu.Lock()
	s.creditsCancel, s.creditsDone = cancel, done
	s.mu.Unlock()
	every := g.RenewEvery
	if every <= 0 {
		every = inference.DefaultRenewEvery
	}
	go func() {
		defer close(done)
		wait := every
		said := false
		for {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			id := s.creditsGrant().Session
			_, err := s.opts.Credits.Renew(ctx, id)
			switch {
			case err == nil:
				said, wait = false, every
			case ctx.Err() != nil:
				return
			case errors.Is(err, inference.ErrEnded):
				if rerr := s.reopenCredits(ctx); rerr != nil {
					s.noteCredits("the Agentry credits session ended and could not be opened again, so the model's calls are refused until it can: " + rerr.Error())
					wait = min(time.Minute, every)
					continue
				}
				said, wait = false, every
			default:
				if !said {
					s.noteCredits(fmt.Sprintf("could not renew the Agentry credits session (trying again each minute): %v", err))
				}
				said, wait = true, min(time.Minute, every)
			}
		}
	}()
}

// reopenCredits opens a new credits session for the agent and starts the
// next segment on its token.
func (s *Supervisor) reopenCredits(ctx context.Context) error {
	if err := s.openCredits(ctx); err != nil {
		return err
	}
	s.turn.Lock()
	defer s.turn.Unlock()
	return s.nextSegmentFor(ctx, "The Agentry credits session was opened again: the agent restarts on it, and the conversation continues")
}

// noteCredits tells the agent's watchers about its credits session.
func (s *Supervisor) noteCredits(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcast(session.Event{Kind: session.KindNotice, Time: time.Now().UTC(), Text: text})
}

// stopCredits stops the renewal and ends the agent's credits session, once
// its last segment has ended, so every token is in the session's count.
func (s *Supervisor) stopCredits() {
	s.mu.Lock()
	cancel, done, g := s.creditsCancel, s.creditsDone, s.credits
	s.creditsCancel, s.creditsDone = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if g == nil || s.opts.Credits == nil {
		return
	}
	ctx, cancelEnd := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelEnd()
	if _, err := s.opts.Credits.End(ctx, g.Session); err != nil && s.opts.Stderr != nil {
		_, _ = fmt.Fprintf(s.opts.Stderr, "agentry: could not end the agent's Agentry credits session (it lapses within the hour): %v\n", err)
	}
}

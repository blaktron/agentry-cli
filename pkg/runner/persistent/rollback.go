package persistent

// A change that gives a running agent something new, whose next segment
// cannot start, is taken back and the agent keeps running: the supervisor
// switches off what the change added or switched on since the running
// segment started, starts the segment without it, and says why. Nothing is
// ever switched back on that way. A change that took something away or
// altered what an entry gives is never taken back: switching back on what
// the person turned off would hand the agent what they withdrew, so the
// agent stops as before, keeping their change for the next start.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
)

// StateNotApplied is the state a segment's envelope records for an entry
// whose change was taken back.
const StateNotApplied = "not-applied"

// widened sorts the roster entries of config cur against prev, the config
// the running segment started with: added are the entries on in cur that
// were off or absent in prev; narrowed says an entry on in prev is off or
// gone in cur, or gives something else (its sentence or signature differ).
func widened(a Agent, prev, cur Config) (added []string, narrowed bool) {
	rp, rc := BuildRoster(a, prev, nil), BuildRoster(a, cur, nil)
	sp, sc := configSignatures(a, prev, rp), configSignatures(a, cur, rc)
	for _, e := range rc {
		was, ok := rp.Entry(e.ID)
		switch {
		case e.On && (!ok || !was.On):
			added = append(added, e.ID)
		case ok && was.On && !e.On:
			narrowed = true
		case ok && was.On && (was.Line != e.Line || sp[e.ID] != sc[e.ID]):
			narrowed = true
		}
	}
	for _, e := range rp {
		if _, ok := rc.Entry(e.ID); !ok && e.On {
			narrowed = true
		}
	}
	slices.Sort(added)
	return added, narrowed
}

// rollBack takes back the change the next segment could not start with
// (cause), when that change only added or switched on entries: each is
// switched off, the segment starts without them, and nil says the agent
// runs again. Otherwise, or when that start fails too, the person's change
// is left as they made it, for the next start, and the error says why. The
// caller holds seg, with the old segment ended.
func (s *Supervisor) rollBack(ctx context.Context, cause error) error {
	s.mu.Lock()
	prev, ok := s.started, s.hasStarted
	s.mu.Unlock()
	if !ok {
		return cause
	}
	a := s.opts.Agent
	reason := failureReason(cause.Error())
	var (
		added []string
		// was is each switched-off entry's switch before, unset when it had
		// none, to put back if the start without them fails too.
		was     map[string]*bool
		refused error
	)
	if err := s.updateConfig(func(c *Config) error {
		var narrowed bool
		added, narrowed = widened(a, prev, *c)
		switch {
		case narrowed:
			refused = fmt.Errorf("%w (the change took something away or changed it, which is never put back without you)", cause)
			return nil
		case len(added) == 0:
			refused = cause
			return nil
		}
		was = map[string]*bool{}
		if c.Switches == nil {
			c.Switches = map[string]bool{}
		}
		for _, id := range added {
			if v, ok := c.Switches[id]; ok {
				was[id] = &v
			} else {
				was[id] = nil
			}
			c.Switches[id] = false
		}
		c.NotApplied = &NotApplied{At: time.Now().UTC(), Reason: reason, Entries: added}
		return nil
	}); err != nil {
		return errors.Join(cause, err)
	}
	if refused != nil {
		return refused
	}
	if err := s.startSegment(ctx, true); err != nil {
		_ = s.updateConfig(func(c *Config) error {
			for id, v := range was {
				if v == nil {
					delete(c.Switches, id)
				} else {
					c.Switches[id] = *v
				}
			}
			c.NotApplied = nil
			return nil
		})
		return &rollBackError{change: cause, without: err}
	}
	s.logf("the change could not be applied (%s), so the agent goes on without it: %s", joinIDs(added), reason)
	s.notice("A change could not be applied, so the agent goes on without " + joinIDs(added) + ", now switched off: " + reason)
	return nil
}

// notApplied marks, in a segment's envelope roster, the entries whose
// change the agent took back and that are still off.
func notApplied(rec []receipt.RosterEntry, c Config) []receipt.RosterEntry {
	if c.NotApplied == nil {
		return rec
	}
	for i, e := range rec {
		if !e.On && slices.Contains(c.NotApplied.Entries, e.ID) {
			rec[i].State = StateNotApplied
		}
	}
	return rec
}

// joinIDs names roster entries for a person: their ids, comma-separated.
func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}

// rollBackError is a change the next segment could not start with, when
// the segment without what it added could not start either.
type rollBackError struct{ change, without error }

func (e *rollBackError) Error() string {
	return e.change.Error() + "; and the agent could not start without the change either: " + e.without.Error()
}

func (e *rollBackError) Unwrap() []error { return []error{e.change, e.without} }

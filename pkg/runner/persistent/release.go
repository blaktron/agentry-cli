package persistent

// A published agent as it is installed now. Installing a newer release of a
// running agent lays its folder down beside the running one and records it;
// the supervisor takes the record at the next change that asks for it and
// at each segment's start, so the next turn starts a segment from the new
// release, the conversation continuing. A release a segment cannot start
// on is set aside: the agent goes on with the release it had, saying why.

import (
	"fmt"
)

// agent is the agent as the supervisor holds it now: the published release
// it last took, or the built-in. It has its own lock, so it is safe to call
// with or without the supervisor's lock held; a function that needs one view
// of the agent calls it once. A supervisor made without Start holds the
// agent it was given.
func (s *Supervisor) agent() Agent {
	s.agMu.Lock()
	defer s.agMu.Unlock()
	if s.ag.Name == "" {
		return s.opts.Agent
	}
	return s.ag
}

// releaseDigest is a published agent's release digest, or "" for a built-in.
func releaseDigest(a Agent) string {
	if a.Release == nil || a.Release.Meta == nil {
		return ""
	}
	return a.Release.Meta.Digest
}

// installedRelease is the published agent's release as its install record
// names it now, when it is not the one held and was not set aside; ok is
// false otherwise.
func (s *Supervisor) installedRelease() (Agent, bool, error) {
	cur := s.agent()
	if !cur.Published() {
		return Agent{}, false, nil
	}
	r, err := LoadRelease(s.opts.Home, cur.Name)
	if err != nil {
		return Agent{}, false, err
	}
	s.agMu.Lock()
	refused := s.agRefused
	s.agMu.Unlock()
	if r.Digest == releaseDigest(cur) || r.Digest == refused {
		return Agent{}, false, nil
	}
	next, err := agentOf(s.opts.Home, cur.Name, r)
	if err != nil {
		// Set aside until another release is recorded, so the agent is
		// not restarted onto it at every turn.
		s.agMu.Lock()
		s.agRefused = r.Digest
		s.agMu.Unlock()
		return Agent{}, false, err
	}
	return next, true, nil
}

// releaseMoved says the install record names a release the running segment
// was not started from, which the next turn starts a segment from.
func (s *Supervisor) releaseMoved() bool {
	cur := s.agent()
	if !cur.Published() {
		return false
	}
	r, err := LoadRelease(s.opts.Home, cur.Name)
	if err != nil {
		return false
	}
	s.agMu.Lock()
	refused := s.agRefused
	s.agMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.Digest != s.segRelease && r.Digest != refused
}

// reloadRelease takes the release the install record names, when it moved:
// what changes and what is agreed to are judged against it from here. A
// record that cannot be read leaves the release held, and is said.
func (s *Supervisor) reloadRelease() {
	next, ok, err := s.installedRelease()
	if err != nil {
		msg := fmt.Sprintf("The %s agent's newly installed release cannot be read, so it goes on with the one it has: %v", s.opts.Agent.Name, err)
		s.logf("%s", msg)
		s.notice(msg)
		return
	}
	if !ok {
		return
	}
	s.agMu.Lock()
	prev := s.ag
	s.ag = next
	if s.agTaken == nil {
		s.agTaken = &prev
	}
	s.agMu.Unlock()
	s.logf("the %s agent takes its release %s@%s", next.Name, next.Ref, next.Version)
}

// revertRelease puts back the release held before one taken since the last
// segment started, when a segment could not start on the new one, and sets
// the new one aside until another is installed. It answers false when no
// release was taken.
func (s *Supervisor) revertRelease(cause error) bool {
	s.agMu.Lock()
	prev := s.agTaken
	refused := s.ag
	if prev == nil {
		s.agMu.Unlock()
		return false
	}
	s.ag, s.agTaken, s.agRefused = *prev, nil, releaseDigest(refused)
	s.agMu.Unlock()
	msg := fmt.Sprintf("The update to %s@%s could not be started, so the agent goes on with %s: %v", refused.Ref, refused.Version, prev.Version, cause)
	s.logf("%s", msg)
	s.notice(msg)
	return true
}

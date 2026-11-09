package persistent

// The runner's policy as a running agent holds it. The supervisor reads
// the policy file at each segment's start, so a change to the model, the
// harness, the malware check, messengers, images or limits reaches a
// running agent at its next turn: a policy file whose digest differs from
// the running segment's makes the agent pending, as a changed roster does.
// A new policy is prepared before it is taken: the credits session a model
// that pays with Agentry credits needs is opened first, and a policy that
// cannot be read, cannot open its session, or cannot start a segment is
// set aside, the agent going on with the policy it had and saying why.
//
// The policy an agent holds is the one its saved model reads
// (policy.ForRun): the model its configuration names, else the default.
// A change of that model is taken the same way, at the next segment. A
// model the computer does not have, or whose credential cannot be used
// here, is never replaced by the default or by the model the agent had:
// the turn that would run on it fails, saying so, and the next tries again.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// heldPolicy is a policy the supervisor holds, with what follows from it:
// full is the policy as it was read, pol the view of it the agent's saved
// model reads, and ref the model reference that view was made for.
type heldPolicy struct {
	full    *policy.Policy
	pol     *policy.Policy
	sha     string
	ref     string
	adapter string
	model   string
	client  *inference.Client
	credits *engine.InferenceGrant
}

// heldKey names a policy file's digest and an agent's model reference
// together: either changing is a policy the agent has not taken.
func heldKey(sha, ref string) string { return sha + "\n" + ref }

// ModelError is a start or a turn refused for the saved model the agent
// names: one the computer does not have, or whose credential cannot be
// used here. Nothing runs on another model in its place.
type ModelError struct {
	Agent string
	// Ref is the saved model the agent names (an id), empty for the
	// default.
	Ref string
	Err error
}

func (e *ModelError) Error() string { return e.Err.Error() }
func (e *ModelError) Unwrap() error { return e.Err }

// isModelError says err refuses the agent's saved model.
func isModelError(err error) bool {
	var me *ModelError
	return errors.As(err, &me)
}

// agentModel is the saved model agent a, naming ref, runs on under the
// policy full: its view of the policy and its harness, as an image. A
// model full does not have, or one usable refuses, is a ModelError.
func agentModel(a string, full *policy.Policy, ref string, usable func(localrun.RunModel) error) (localrun.RunModel, error) {
	rm, err := localrun.ChooseRunModel(full, ref, "")
	if err != nil {
		if localrun.IsMissingModel(err) {
			err = fmt.Errorf("the %s agent names the saved model %q, which this computer does not have, and an agent never runs on the default in its place: move it to one it has (agentry agent configure %s --model <name>, or --model default; agentry models list)", a, ref, a)
		}
		return rm, engine.Classify(engine.ExitPolicy, &ModelError{Agent: a, Ref: ref, Err: err})
	}
	rm.Adapter = localrun.ImageAdapter(rm.Adapter)
	if usable != nil {
		if err := usable(rm); err != nil {
			return rm, engine.Classify(engine.ExitPolicy, &ModelError{Agent: a, Ref: ref, Err: err})
		}
	}
	return rm, nil
}

// policy is the policy the supervisor holds now.
func (s *Supervisor) policy() *policy.Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pol
}

// policySHA is the digest of the policy file the supervisor holds.
func (s *Supervisor) policySHA() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polSHA
}

// policyPath is the policy file the agent was started with, or "" when the
// client gave none (its policy then never changes).
func (s *Supervisor) policyPath() string {
	if s.opts.PolicyFile == nil {
		return ""
	}
	return s.opts.PolicyFile.Path
}

// modelLabel is how a policy's model is named: provider/name.
func modelLabel(p *policy.Policy) string {
	if p == nil {
		return ""
	}
	return p.Model.Provider + "/" + p.Model.Name
}

// savedModelLabel is how the saved model a policy's view reads is named to
// the person: its name, then provider/name.
func savedModelLabel(p *policy.Policy) string {
	if p == nil {
		return ""
	}
	if sm, ok := p.RunSavedModel(); ok && sm.Name != "" {
		return sm.Name + " (" + modelLabel(p) + ")"
	}
	return modelLabel(p)
}

// heldModel is the saved model the agent's held policy reads; false for a
// policy that has none.
func (s *Supervisor) heldModel() (policy.SavedModel, bool) {
	p := s.policy()
	if p == nil {
		return policy.SavedModel{}, false
	}
	return p.RunSavedModel()
}

// candidatePolicy is the policy file as it is now, when it or the agent's
// model reference ref differs from what the supervisor holds and was not
// set aside; nil otherwise. The file is read again only when its size or
// time changed since it was last read.
func (s *Supervisor) candidatePolicy(ref string) (*policy.Policy, string, error) {
	path := s.policyPath()
	if path == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if ref == s.polRef || s.full == nil || heldKey(s.polSHA, ref) == s.polRefused {
			return nil, "", nil
		}
		return s.full, s.polSHA, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	seen := s.polSeen
	s.mu.Unlock()
	if seen.size != fi.Size() || !seen.mod.Equal(fi.ModTime()) || (seen.pol == nil && seen.err == nil) {
		p, sha, err := policy.Load(path)
		seen = statSeen{size: fi.Size(), mod: fi.ModTime(), pol: p, sha: sha, err: err}
		s.mu.Lock()
		s.polSeen = seen
		s.mu.Unlock()
	}
	if seen.err != nil {
		return nil, "", seen.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if (seen.sha == s.polSHA && ref == s.polRef) || heldKey(seen.sha, ref) == s.polRefused {
		return nil, "", nil
	}
	return seen.pol, seen.sha, nil
}

// statSeen is the policy file as the supervisor last read it: its size and
// time, and what reading it gave.
type statSeen struct {
	size int64
	mod  time.Time
	pol  *policy.Policy
	sha  string
	err  error
}

// policyMoved says the policy file, or the agent's model reference ref,
// gives a policy the supervisor does not hold and has not set aside. A file
// that cannot be read has not moved.
func (s *Supervisor) policyMoved(ref string) bool {
	p, _, err := s.candidatePolicy(ref)
	return err == nil && p != nil
}

// checkModel says whether the next segment could start on the saved model
// the agent names now (ref): nil when the policy it would take has that
// model and it can be used, or when there is no policy to take; a
// ModelError otherwise. Nothing changes.
func (s *Supervisor) checkModel(ref string) error {
	p, _, err := s.candidatePolicy(ref)
	if err != nil || p == nil {
		return nil
	}
	_, err = agentModel(s.opts.Agent.Name, p, ref, s.opts.ModelUsable)
	return err
}

// reloadPolicy takes the policy file as it is now, before a segment starts.
// The candidate is prepared first: a model that pays with Agentry credits
// opens its session with a client for its own registry, before anything
// changes. Only then is it taken, the harness and the model following, the
// old credits session ended, messengers held to it and the person told what
// changed. A candidate that cannot be read or prepared is set aside and
// said, and the agent keeps the policy it has. A saved model it cannot run
// on is not set aside: the ModelError is returned, for the segment to fail
// with, and the next segment tries again. The caller holds seg.
func (s *Supervisor) reloadPolicy(ctx context.Context, ref string) error {
	p, sha, err := s.candidatePolicy(ref)
	if err != nil {
		s.logf("the policy %s cannot be read, so the agent keeps the one it has: %v", s.policyPath(), err)
		return nil
	}
	if p == nil {
		return nil
	}
	next, err := s.preparePolicy(ctx, p, sha, ref)
	if isModelError(err) {
		return err
	}
	if err != nil {
		s.refusePolicy(heldKey(sha, ref), err)
		return nil
	}
	s.takePolicy(next, true)
	return nil
}

// preparePolicy is what holding full, for the agent's model reference
// ref, would be: the view of it that saved model reads, its harness and
// model, and, for a model that pays with Agentry credits, a client for its
// registry and a session opened with it (kept from the policy held when
// the registry and the model are the same).
func (s *Supervisor) preparePolicy(ctx context.Context, full *policy.Policy, sha, ref string) (heldPolicy, error) {
	rm, err := agentModel(s.opts.Agent.Name, full, ref, s.opts.ModelUsable)
	if err != nil {
		return heldPolicy{}, err
	}
	p := rm.Policy
	next := heldPolicy{full: full, pol: p, sha: sha, ref: ref, adapter: rm.Adapter, model: modelLabel(p)}
	if p.Model.Provider != policy.ProviderAgentry {
		return next, nil
	}
	s.mu.Lock()
	cur, curClient, curGrant := s.pol, s.creditsCl, s.credits
	s.mu.Unlock()
	if curGrant != nil && cur != nil && cur.Model.Provider == policy.ProviderAgentry && cur.Model.BaseURL == p.Model.BaseURL && cur.Model.Name == p.Model.Name {
		next.client, next.credits = curClient, curGrant
		return next, nil
	}
	if s.opts.CreditsFor != nil {
		next.client = s.opts.CreditsFor(p)
	} else {
		next.client = s.opts.Credits
	}
	g, err := openCreditsWith(ctx, next.client)
	if err != nil {
		return heldPolicy{}, err
	}
	next.credits = g
	return next, nil
}

// takePolicy holds next. The credits session it replaces is ended once the
// renewal of it has stopped, out of the caller's way; say tells the person
// what changed. It returns the policy held before.
func (s *Supervisor) takePolicy(next heldPolicy, say bool) heldPolicy {
	s.mu.Lock()
	prev := heldPolicy{full: s.full, pol: s.pol, sha: s.polSHA, ref: s.polRef, adapter: s.adapter, model: s.model, client: s.creditsCl, credits: s.credits}
	s.full, s.pol, s.polSHA, s.polRef, s.adapter, s.model = next.full, next.pol, next.sha, next.ref, next.adapter, next.model
	s.creditsCl, s.credits = next.client, next.credits
	s.polTaken = &prev
	s.mu.Unlock()
	if prev.credits != nil && (next.credits == nil || prev.credits.Session != next.credits.Session) {
		s.endCreditsLater(prev.client, prev.credits)
	}
	if next.credits != nil && (prev.credits == nil || prev.credits.Session != next.credits.Session) {
		s.startCreditsRenewal()
	}
	if say {
		s.sayPolicyChange(prev, next)
	}
	go s.reconcileMessengers() // the caller holds seg, which reconciling a messenger's role may wait on
	return prev
}

// refusePolicy sets aside the policy file and model reference key names
// (heldKey), which the agent could not take, and says why; the agent keeps
// the policy it has until either changes again.
func (s *Supervisor) refusePolicy(key string, err error) {
	s.mu.Lock()
	s.polRefused = key
	s.mu.Unlock()
	s.logf("the policy changed, and the agent could not take it, so it goes on with the one it had: %v", err)
	s.notice("The runner's policy changed, and the agent could not take it, so it goes on with the one it had: " + err.Error())
}

// revertPolicy puts back the policy held before the last one taken, when a
// segment could not start on the new one, and sets the new one aside. It
// answers false when no policy was taken since the last segment started,
// and when the agent was moved to another saved model since: it never runs
// on the model it was moved from in its place.
func (s *Supervisor) revertPolicy(cause error) bool {
	s.mu.Lock()
	prev, refused := s.polTaken, heldKey(s.polSHA, s.polRef)
	moved := prev != nil && prev.ref != s.polRef
	s.polTaken = nil
	s.mu.Unlock()
	if prev == nil || prev.pol == nil || moved {
		return false
	}
	s.takePolicy(*prev, false)
	s.refusePolicy(refused, fmt.Errorf("a segment could not start on it: %w", cause))
	return true
}

// sayPolicyChange tells the person what a change of policy, or of the
// agent's saved model, does to the agent.
func (s *Supervisor) sayPolicyChange(prev, next heldPolicy) {
	was, now, wasAdapter, nowAdapter := prev.pol, next.pol, prev.adapter, next.adapter
	if was == nil || now == nil {
		return
	}
	var said []string
	if savedModelLabel(was) != savedModelLabel(now) {
		said = append(said, "its model is now "+savedModelLabel(now))
	}
	if wasAdapter != nowAdapter {
		said = append(said, "it runs on "+nowAdapter+" now, in a new conversation that carries the last exchange")
	}
	if was.MalwareCheckOn() != now.MalwareCheckOn() {
		if now.MalwareCheckOn() {
			said = append(said, "what you add to its share is checked for malware with ClamAV")
		} else {
			said = append(said, "what you add to its share is read unchecked")
		}
	}
	text := "The runner's policy changed"
	if prev.sha == next.sha && prev.ref != next.ref {
		text = "The agent's model changed"
	}
	for i, x := range said {
		if i == 0 {
			text += ": " + x
		} else {
			text += "; " + x
		}
	}
	s.logf("%s", text)
	s.notice(text + ".")
}

// adapterName is the harness the supervisor runs segments on.
func (s *Supervisor) adapterName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adapter
}

// policyOutsideShare refuses a policy file the agent itself could write:
// one in its share folder, which it reads and writes, would let it change
// the policy it runs under at its next turn.
func policyOutsideShare(a Agent, c Config, f *policy.File) error {
	if f == nil || f.Path == "" {
		return nil
	}
	share, err := ShareFolder(a, c)
	if err != nil || share == "" {
		return nil
	}
	if rel, err := filepath.Rel(share, f.Path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("the policy %s is in the %s agent's share folder, which the agent writes: keep the policy elsewhere", f.Path, a.Name)
	}
	return nil
}

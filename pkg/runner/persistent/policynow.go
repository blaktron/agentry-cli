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

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/runner/adapter"
	"github.com/blaktron/agentry-cli/pkg/runner/engine"
	"github.com/blaktron/agentry-cli/pkg/runner/inference"
	"github.com/blaktron/agentry-cli/pkg/runner/localrun"
	"github.com/blaktron/agentry-cli/pkg/runner/policy"
)

// heldPolicy is a policy the supervisor holds, with what follows from it.
type heldPolicy struct {
	pol     *policy.Policy
	sha     string
	adapter string
	model   string
	client  *inference.Client
	credits *engine.InferenceGrant
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

// candidatePolicy is the policy file as it is now, when it differs from the
// one the supervisor holds and was not set aside; nil otherwise. The file
// is read again only when its size or time changed since it was last read.
func (s *Supervisor) candidatePolicy() (*policy.Policy, string, error) {
	path := s.policyPath()
	if path == "" {
		return nil, "", nil
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
	if seen.sha == s.polSHA || seen.sha == s.polRefused {
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

// policyMoved says the policy file holds a policy the supervisor does not
// and has not set aside. A file that cannot be read has not moved.
func (s *Supervisor) policyMoved() bool {
	p, _, err := s.candidatePolicy()
	return err == nil && p != nil
}

// reloadPolicy takes the policy file as it is now, before a segment starts.
// The candidate is prepared first: a model that pays with Agentry credits
// opens its session with a client for its own registry, before anything
// changes. Only then is it taken, the harness and the model following, the
// old credits session ended, messengers held to it and the person told what
// changed. A candidate that cannot be read or prepared is set aside and
// said, and the agent keeps the policy it has. The caller holds seg.
func (s *Supervisor) reloadPolicy(ctx context.Context) {
	p, sha, err := s.candidatePolicy()
	if err != nil {
		s.logf("the policy %s cannot be read, so the agent keeps the one it has: %v", s.policyPath(), err)
		return
	}
	if p == nil {
		return
	}
	next, err := s.preparePolicy(ctx, p, sha)
	if err != nil {
		s.refusePolicy(sha, err)
		return
	}
	s.takePolicy(next, true)
}

// preparePolicy is what holding p would be: its harness and model, and,
// for a model that pays with Agentry credits, a client for its registry and
// a session opened with it (kept from the policy held when the registry and
// the model are the same).
func (s *Supervisor) preparePolicy(ctx context.Context, p *policy.Policy, sha string) (heldPolicy, error) {
	next := heldPolicy{pol: p, sha: sha, adapter: localrun.ImageAdapter(adapter.DefaultFor(p)), model: modelLabel(p)}
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
	prev := heldPolicy{pol: s.pol, sha: s.polSHA, adapter: s.adapter, model: s.model, client: s.creditsCl, credits: s.credits}
	s.pol, s.polSHA, s.adapter, s.model = next.pol, next.sha, next.adapter, next.model
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
		s.sayPolicyChange(prev.pol, next.pol, prev.adapter, next.adapter)
	}
	go s.reconcileMessengers() // the caller holds seg, which reconciling a messenger's role may wait on
	return prev
}

// refusePolicy sets aside the policy file whose digest is sha, which the
// agent could not take, and says why; the agent keeps the policy it has
// until the file changes again.
func (s *Supervisor) refusePolicy(sha string, err error) {
	s.mu.Lock()
	s.polRefused = sha
	s.mu.Unlock()
	s.logf("the policy changed, and the agent could not take it, so it goes on with the one it had: %v", err)
	s.notice("The runner's policy changed, and the agent could not take it, so it goes on with the one it had: " + err.Error())
}

// revertPolicy puts back the policy held before the last one taken, when a
// segment could not start on the new one, and sets the new one aside. It
// answers false when no policy was taken since the last segment started.
func (s *Supervisor) revertPolicy(cause error) bool {
	s.mu.Lock()
	prev, refused := s.polTaken, s.polSHA
	s.polTaken = nil
	s.mu.Unlock()
	if prev == nil || prev.pol == nil {
		return false
	}
	s.takePolicy(*prev, false)
	s.refusePolicy(refused, fmt.Errorf("a segment could not start on it: %w", cause))
	return true
}

// sayPolicyChange tells the person what a policy change does to the agent.
func (s *Supervisor) sayPolicyChange(was, now *policy.Policy, wasAdapter, nowAdapter string) {
	if was == nil || now == nil {
		return
	}
	var said []string
	if modelLabel(was) != modelLabel(now) {
		said = append(said, "its model is now "+modelLabel(now))
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

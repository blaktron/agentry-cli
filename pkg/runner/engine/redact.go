package engine

import (
	"errors"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"os"
	"sort"
	"strings"
)

// redactor masks the values that must never reach a receipt: the run's
// gateway token, every resolved secret value (the agent's and the declared
// servers', the same set writeSecretsFile writes) and the model API key
// (§R11 invariant 7). A session applies it to its transcript and events;
// a run and a session both apply it to the gateway and proxy logs, the
// agent's stderr receipt, the result text and the run's error — a
// declared server that prints its environment on failure would otherwise
// put a secret into gateway.log and, through the quoted tail, into
// result.json (agentry-cli#109).
type redactor struct{ values []string }

// redactMinLen is the shortest value the redactor masks: a shorter one
// (`none`, `true`, a port) is a switch, not a credential, and masking it
// would garble every receipt that happens to contain the word.
const redactMinLen = 8

func newRedactor(token string, secrets map[string]string, extra []string) *redactor {
	var vals []string
	add := func(v string) {
		if len(v) >= redactMinLen {
			vals = append(vals, v)
		}
	}
	add(token)
	for _, v := range secrets {
		add(v)
	}
	for _, v := range extra {
		add(v)
	}
	// Longest first so a value that contains another is masked whole.
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	return &redactor{values: vals}
}

// newRedactor builds the run's redactor from the values a run knows once
// its token and secrets exist, and hangs it off the run's directories so
// every later phase masks with the same one.
func (rd *runDirs) newRedactor(p Params, token string, secrets map[string]string) *redactor {
	// Both harness kinds: the agent container's env and, for a host-run
	// harness (claude, codex), the process env the key travels in.
	vars := append([]spec.EnvVar(nil), p.Plan.Harness.Agent.Env...)
	if p.Plan.Harness.Host != nil {
		vars = append(vars, p.Plan.Harness.Host.Env...)
	}
	rd.redact = newRedactor(token, secrets, hostSecretValues(vars))
	return rd.redact
}

// String masks every known value in s.
func (r *redactor) String(s string) string {
	if r == nil {
		return s
	}
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

// Err masks every known value in an error's text, keeping its exit class.
// An error whose text carries nothing to mask is returned as is, so
// callers' errors.Is checks still see it.
func (r *redactor) Err(err error) error {
	if r == nil || err == nil {
		return err
	}
	msg := err.Error()
	masked := r.String(msg)
	if masked == msg {
		return err
	}
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		return Classify(ce.Code, errors.New(masked))
	}
	return errors.New(masked)
}

// File rewrites a receipt file with every known value masked, and leaves
// it untouched — byte for byte — when there is nothing to mask. Used for
// the agent's stderr, which the harness process writes directly.
func (r *redactor) File(path string) {
	if r == nil || len(r.values) == 0 {
		return
	}
	b, err := os.ReadFile(path) //nolint:gosec // our receipts file
	if err != nil {
		return
	}
	masked := r.String(string(b))
	if masked == string(b) {
		return
	}
	_ = os.WriteFile(path, []byte(masked), 0o644)
}

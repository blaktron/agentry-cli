package engine

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/blaktron/agentry-cli/pkg/runner/spec"
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
// A token renewed during the run (plan D24) is added while other
// goroutines mask, so the values are guarded.
type redactor struct {
	mu     sync.RWMutex
	values []string
}

// gatewayMask is how docker/mcp-gateway (v0.43.3, pkg/gateway/clientpool.go
// and pkg/mcp/remote.go maskSecret) lists a secret it injects at a server's
// start, verbose being its default: "    - NAME: " + the value's first four
// characters + "****". Four characters are no whole value, so the redactor
// masks this form of every value it holds as well, and the line keeps the
// name and none of the value (agentry-cli#372). A value shorter than
// redactMinLen is held by neither form, and is named in the receipt instead
// (withRedactionNote); redactMinLen >= 4 keeps the slice in range.
func gatewayMask(v string) string { return v[:4] + "****" }

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
	return &redactor{values: maskOrder(vals)}
}

// maskOrder lays out the values the redactor masks: the whole values
// longest first, so a value that contains another is masked whole, then
// the gateway's partial mask of each, so none of them can split a value
// that is masked whole. A redactor's values are always this layout, whole
// values in the first half.
func maskOrder(whole []string) []string {
	whole = slices.Clone(whole)
	sort.Slice(whole, func(i, j int) bool { return len(whole[i]) > len(whole[j]) })
	vals := slices.Clone(whole)
	for _, v := range whole {
		vals = append(vals, gatewayMask(v))
	}
	return vals
}

// add masks one more value from now on: a connection's access token
// renewed mid-run. Nil-safe, as the rest.
func (r *redactor) add(v string) {
	if r == nil || len(v) < redactMinLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	whole := r.values[:len(r.values)/2]
	if slices.Contains(whole, v) {
		return
	}
	r.values = maskOrder(append(slices.Clone(whole), v))
}

// withRedactionNote names, in the receipt's enforced capabilities, every
// declared secret whose resolved value is too short for the redactor to
// mask, so the receipt never implies a mask it did not apply
// (agentry-cli#164). Declared names only: the value is the thing not being
// masked. secrets is secretValues' map, keyed as the gateway reads it.
func withRedactionNote(enforced map[string]string, p Params, secrets map[string]string) map[string]string {
	short := map[string]bool{}
	check := func(name, key string) {
		if v := secrets[key]; v != "" && len(v) < redactMinLen {
			short[name] = true
		}
	}
	eachPlanSecret(p.Plan, check)
	if len(short) == 0 {
		return enforced
	}
	names := slices.Sorted(maps.Keys(short))
	out := maps.Clone(enforced)
	if out == nil {
		out = map[string]string{}
	}
	out["redaction"] = fmt.Sprintf("secret values shorter than %d characters are not masked in receipts: %s", redactMinLen, strings.Join(names, ", "))
	return out
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
	// A target host's sign-in is the run's own credential too (D19).
	rd.redact = newRedactor(token, secrets, append(hostSecretValues(vars), hostCredentialValues(p)...))
	return rd.redact
}

// String masks every known value in s.
func (r *redactor) String(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	if r == nil {
		return
	}
	r.mu.RLock()
	empty := len(r.values) == 0
	r.mu.RUnlock()
	if empty {
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

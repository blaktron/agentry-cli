package engine

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
)

// wantRefused holds a refused run's envelope to the inputs it names.
func wantRefused(t *testing.T, env *receipt.Envelope, want []receipt.RefusedInput, more int) {
	t.Helper()
	if env == nil || !reflect.DeepEqual(env.RefusedInputs, want) || env.RefusedInputsMore != more {
		t.Fatalf("refused inputs %+v (and %d more), want %+v (and %d more)", env.RefusedInputs, env.RefusedInputsMore, want, more)
	}
}

// wantReason holds a failed run's envelope to the reason the step that
// stopped it decided.
func wantReason(t *testing.T, env *receipt.Envelope, err error, want *receipt.Reason) {
	t.Helper()
	if err == nil || env == nil || env.Status != "error" {
		t.Fatalf("err %v, envelope %+v: want a failed run", err, env)
	}
	if !reflect.DeepEqual(env.Reason, want) {
		var gotCount, wantCount any
		if env.Reason != nil && env.Reason.Count != nil {
			gotCount = *env.Reason.Count
		}
		if want != nil && want.Count != nil {
			wantCount = *want.Count
		}
		t.Fatalf("reason %+v (count %+v), want %+v (count %+v) (err %v)", env.Reason, gotCount, want, wantCount, err)
	}
}

func TestAMissingSecretIsNamedInTheReason(t *testing.T) {
	p, f := fixture(t)
	_ = os.Unsetenv("ENGINE_TEST_TOKEN")
	env, err := Run(context.Background(), f, p)
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonSecretMissing, Names: []string{"ENGINE_TEST_TOKEN"}})
	wantRefused(t, env, nil, 0)
}

func TestADecodeThatCannotRunSaysSoInTheReason(t *testing.T) {
	resetDecodeCalls()
	p, f := fixture(t)
	happyFake(f)
	p.InputPaths = []string{writeInput(t, t.TempDir(), "report.pdf", "%PDF-1.7")}
	decodeCalls.Lock()
	decodeCalls.fail = errors.New("could not pull the files image")
	decodeCalls.Unlock()
	env, err := Run(context.Background(), f, p)
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonInputsNotDecoded, Detail: receipt.DecodeImage})
}

// The reason says malware, and never which file: the name is the
// person's. The envelope's refusedInputs names it, in typed fields, for a
// driver that keeps no text.
func TestMalwareInAnInputIsTheReasonWithNoFileNamed(t *testing.T) {
	resetDecodeCalls()
	p, f := fixture(t)
	happyFake(f)
	p.InputPaths = []string{writeInput(t, t.TempDir(), "eicar.com", "X5O EICAR")}
	env, err := Run(context.Background(), f, p)
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonInputRefused, Detail: receipt.InputMalware})
	wantRefused(t, env, []receipt.RefusedInput{{Path: "eicar.com", By: receipt.RefusedByClamAV, Signature: "Eicar-Test-Signature"}}, 0)
}

// An envelope names at most five refused inputs and counts the rest, and
// leaves out a path that carries a value the run masks.
func TestARefusalNamesFiveInputsAndCountsTheRest(t *testing.T) {
	const token = "agentry-gateway-token-0123456789"
	var in []receipt.RefusedInput
	for _, name := range []string{"a.com", "b.com", "c.com", token + ".com", "d.com", "e.com", "f.com"} {
		in = append(in, receipt.RefusedInput{Path: name, By: receipt.RefusedByClamAV, Signature: "Eicar-Test-Signature"})
	}
	env := &receipt.Envelope{}
	err := Classify(ExitPolicy, receipt.WithRefusedInputs(inputRefused(receipt.InputMalware, errors.New("refused")), in))
	_, _ = fail(env, newRedactor(token, nil, nil), err)
	var paths []string
	for _, r := range env.RefusedInputs {
		paths = append(paths, r.Path)
	}
	if strings.Join(paths, " ") != "a.com b.com c.com d.com" || env.RefusedInputsMore != 3 {
		t.Fatalf("named %v and %d more", paths, env.RefusedInputsMore)
	}
}

func TestARefusedInputSaysWhyInTheReason(t *testing.T) {
	t.Run("two inputs share a name", func(t *testing.T) {
		p, f := fixture(t)
		happyFake(f)
		p.InputPaths = []string{writeInput(t, t.TempDir(), "notes.txt", "a"), writeInput(t, t.TempDir(), "notes.txt", "b")}
		env, err := Run(context.Background(), f, p)
		wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonInputRefused, Detail: receipt.InputNameClash})
	})
	t.Run("an input that is not there", func(t *testing.T) {
		p, f := fixture(t)
		happyFake(f)
		p.InputPaths = []string{t.TempDir() + "/missing.txt"}
		env, err := Run(context.Background(), f, p)
		wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonInputRefused, Detail: receipt.InputUnreadable})
	})
}

func TestADeclaredServerThatDidNotStartIsNamed(t *testing.T) {
	servers := []spec.MCPServerSpec{{Name: "mcp-npm-scope-server-x", Source: "npm", Package: "@scope/server-x", Version: "1.0.0"}}
	err := checkDeclaredServersStarted("  > Can't start mcp-npm-scope-server-x: failed to connect: EOF\n", servers)
	want := &receipt.Reason{Code: receipt.ReasonMCPStartFailed, Names: []string{"npm:@scope/server-x"}}
	if got := receipt.ReasonOf(err); !reflect.DeepEqual(got, want) {
		t.Fatalf("reason %+v, want %+v (err %v)", got, want, err)
	}
}

// The redactor rebuilds a masked error without its chain; the reason is
// read before it, and the error's text is still masked.
func TestTheReasonOutlivesTheRedactor(t *testing.T) {
	const token = "agentry-gateway-token-0123456789"
	env := &receipt.Envelope{}
	err := Classify(ExitGateway, withRefusals(receipt.WithReason(errors.New("server said "+token), receipt.ReasonMCPStartFailed, "", "pypi:osv-mcp"), nil))
	_, ce := fail(env, newRedactor(token, nil, nil), err)
	if strings.Contains(env.Error, token) || ce == nil {
		t.Fatalf("the error kept the token: %q", env.Error)
	}
	want := &receipt.Reason{Code: receipt.ReasonMCPStartFailed, Names: []string{"pypi:osv-mcp"}}
	if !reflect.DeepEqual(env.Reason, want) {
		t.Fatalf("reason %+v, want %+v", env.Reason, want)
	}
}

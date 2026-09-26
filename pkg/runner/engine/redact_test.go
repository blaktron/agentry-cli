package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry-cli/runnerimage/toolsrv"
)

// TestRedactorErrKeepsClassAndIdentity: an error that carries a value is
// re-issued masked under the same exit class; one that carries none is
// returned as is, so errors.Is still sees it.
func TestRedactorErrKeepsClassAndIdentity(t *testing.T) {
	// B is seven characters and stays (a switch, not a credential); C is
	// eight and is masked: the boundary of redactMinLen.
	r := newRedactor("tok-0123456789", map[string]string{"A": "sekret-value-1234", "B": "seven77", "C": "eight888"}, []string{"sk-host-key-9876"})
	base := errors.New("gateway said: token=tok-0123456789 A=sekret-value-1234 B=seven77 C=eight888 K=sk-host-key-9876")
	got := r.Err(Classify(ExitGateway, base))
	var ce *ClassifiedError
	if !errors.As(got, &ce) || ce.Code != ExitGateway {
		t.Fatalf("class lost: %v", got)
	}
	if want := "gateway said: token=[redacted] A=[redacted] B=seven77 C=[redacted] K=[redacted]"; got.Error() != want {
		t.Errorf("Err = %q, want %q", got.Error(), want)
	}
	clean := Classify(ExitHarness, errors.New("nothing here"))
	if r.Err(clean) != clean {
		t.Error("an error with nothing to mask must be returned as is")
	}
	if r.Err(nil) != nil || (*redactor)(nil).Err(clean) != clean {
		t.Error("nil redactor or nil error must pass through")
	}
}

// TestRedactorFile: a receipt file with a value is rewritten masked; one
// without is left byte-identical (its mtime included: no write).
func TestRedactorFile(t *testing.T) {
	r := newRedactor("tok-0123456789", map[string]string{"A": "sekret-value-1234"}, nil)
	dir := t.TempDir()
	leaky := filepath.Join(dir, "agent-stderr.log")
	if err := os.WriteFile(leaky, []byte("boot\nAuthorization: Bearer sekret-value-1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.File(leaky)
	if b, _ := os.ReadFile(leaky); string(b) != "boot\nAuthorization: Bearer [redacted]\n" {
		t.Errorf("leaky file = %q", b)
	}
	clean := filepath.Join(dir, "clean.log")
	if err := os.WriteFile(clean, []byte("boot\nall fine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(clean)
	r.File(clean)
	after, _ := os.Stat(clean)
	if b, _ := os.ReadFile(clean); string(b) != "boot\nall fine\n" || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("clean file was rewritten: %q", b)
	}
	r.File(filepath.Join(dir, "absent.log")) // no receipt, no panic
}

// TestShortSecretsAreDisclosedInTheReceipt: a declared secret too short for
// the redactor to mask is named in the receipt's enforced capabilities, so
// the receipt never implies a mask it did not apply (agentry-cli#164).
func TestShortSecretsAreDisclosedInTheReceipt(t *testing.T) {
	p, f := fixture(t)
	happyFake(f)
	p.SecretOverrides = map[string]string{"ENGINE_TEST_TOKEN": "x9q"}
	env, err := Run(context.Background(), f, p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := env.CapabilitiesEnforced["redaction"]
	if !strings.Contains(got, "ENGINE_TEST_TOKEN") || strings.Contains(got, "x9q") {
		t.Errorf("redaction = %q, want the secret's name and never its value", got)
	}

	p2, f2 := fixture(t)
	happyFake(f2)
	p2.SecretOverrides = map[string]string{"ENGINE_TEST_TOKEN": "long-enough-value"}
	env2, err := Run(context.Background(), f2, p2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, ok := env2.CapabilitiesEnforced["redaction"]; ok {
		t.Errorf("redaction = %q with every secret long enough, want no entry", got)
	}
}

// TestRedactionNoteReadsServerScopedSecrets: a declared server's short
// secret is named by its declared name; a name the agent and a server both
// declare appears once; a long value and an existing entry are left alone.
func TestRedactionNoteReadsServerScopedSecrets(t *testing.T) {
	var p Params
	p.Plan = &spec.Plan{
		Secrets:    []string{"SHARED", "LONG_ONE"},
		MCPServers: []spec.MCPServerSpec{{Name: "mcp-x", Secrets: []string{"SHARED", "SRV_PIN"}}},
	}
	secrets := map[string]string{
		compile.SecretKey("SHARED"):                 "abc",
		compile.SecretKey("LONG_ONE"):               "long-enough-value",
		compile.ServerSecretKey("mcp-x", "SHARED"):  "abc",
		compile.ServerSecretKey("mcp-x", "SRV_PIN"): "1234",
	}
	in := map[string]string{"network": "internal"}
	out := withRedactionNote(in, p, secrets)
	want := "secret values shorter than 8 characters are not masked in receipts: SHARED, SRV_PIN"
	if out["redaction"] != want || out["network"] != "internal" {
		t.Errorf("note = %v, want redaction %q beside the existing entries", out, want)
	}
	if _, touched := in["redaction"]; touched {
		t.Error("the input map was changed")
	}
}

// TestRedactorMasksTheGatewaysPartialMask (agentry-cli#372): a held value's
// first four characters followed by "****", the gateway's own listing of it,
// is masked whole; a short value is held in neither form, and a bare "****"
// or another value's prefix is left alone.
func TestRedactorMasksTheGatewaysPartialMask(t *testing.T) {
	r := newRedactor("tok-0123456789", map[string]string{"A": "sekret-value-1234", "B": "seven77"}, nil)
	in := "    - A: sekr****\n    - T: tok-****\n    - B: seve****\n    - C: ****\n    - D: xyzw****\nfull sekret-value-1234\n"
	want := "    - A: [redacted]\n    - T: [redacted]\n    - B: seve****\n    - C: ****\n    - D: xyzw****\nfull [redacted]\n"
	if got := r.String(in); got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

// TestGatewaySecretListingKeepsNoCharacterOfTheValue (agentry-cli#372): the
// gateway (docker/mcp-gateway v0.43.3, verbose by default) lists each secret
// it injects at a server's start as "    - NAME: " + the value's first four
// characters + "****" (pkg/gateway/clientpool.go maskSecret). That prefix is
// no whole value, so it must be masked as one: gateway.log keeps the line and
// the name, and no character of the value — for a declared server's secret
// and for the web tools' keys alike.
func TestGatewaySecretListingKeepsNoCharacterOfTheValue(t *testing.T) {
	const serverSecret = "server-sekret-5678"
	listing := func(pairs ...string) string {
		var b strings.Builder
		b.WriteString("- Starting the servers\n")
		for i := 0; i < len(pairs); i += 2 {
			b.WriteString("    - " + pairs[i] + ": " + pairs[i+1][:4] + "****\n")
		}
		return b.String()
	}
	masked := func(names ...string) string {
		var b strings.Builder
		b.WriteString("- Starting the servers\n")
		for _, n := range names {
			b.WriteString("    - " + n + ": [redacted]\n")
		}
		return b.String()
	}
	check := func(t *testing.T, dir, want string, values ...string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "gateway.log"))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("gateway.log = %q, want %q", b, want)
		}
		for _, v := range values {
			if strings.Contains(string(b), v[:4]+"****") {
				t.Errorf("gateway.log keeps %q, the first characters of a secret", v[:4])
			}
		}
	}
	t.Run("declared secrets", func(t *testing.T) {
		p, f := serverFixture(t)
		happyFake(f)
		p.Plan.MCPServers[0].Secrets = []string{"SERVER_TEST_TOKEN"}
		t.Setenv("SERVER_TEST_TOKEN", serverSecret)
		f.LogsByName = map[string]string{"agentry-cafe0000-gateway": listing(
			"ENGINE_TEST_TOKEN", "sekret-value-1234", "SERVER_TEST_TOKEN", serverSecret)}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		check(t, env.Receipts.Dir, masked("ENGINE_TEST_TOKEN", "SERVER_TEST_TOKEN"), "sekret-value-1234", serverSecret)
	})
	t.Run("the web tools' keys", func(t *testing.T) {
		p, f := webFixture(t)
		p.Web.JobToken = testJobToken
		happyFake(f)
		f.LogsByName = map[string]string{"agentry-d4af0000-gateway": listing(
			toolsrv.EnvWebJevKey, testJevKey, toolsrv.EnvWebJobToken, testJobToken, toolsrv.EnvWebSearchKey, testSearchKey)}
		env, err := Run(context.Background(), f, p)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		check(t, env.Receipts.Dir, masked(toolsrv.EnvWebJevKey, toolsrv.EnvWebJobToken, toolsrv.EnvWebSearchKey), testJevKey, testJobToken, testSearchKey)
	})
}

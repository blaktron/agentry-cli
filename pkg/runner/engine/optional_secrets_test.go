package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blaktron/agentry-cli/pkg/runner/compile"
	"github.com/blaktron/agentry-cli/pkg/runner/receipt"
	"github.com/blaktron/agentry-cli/pkg/runner/spec"
	"github.com/blaktron/agentry/pkg/manifest"
)

// Optional secrets (capabilities.optionalSecrets; DR-70, agentry-cli#651,
// plan optional-secrets.md §5). A run starts without an optional secret it
// has no value for: the gateway's secrets file holds it empty, the System
// Prompt and the receipt name it, and a missing required secret is still
// exit 11, naming only the required ones.

func TestAMissingOptionalSecretStartsTheRun(t *testing.T) {
	p, f := fixtureEdited(t, nil, func(m *manifest.Manifest) {
		m.Capabilities.OptionalSecrets = []string{"ENGINE_TEST_TOKEN"}
	})
	happyFake(f)
	_ = os.Unsetenv("ENGINE_TEST_TOKEN")
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("a missing optional secret refused the run: %v (%+v)", err, env)
	}
	if got := env.CapabilitiesEnforced["secrets.absent"]; !strings.HasSuffix(got, ": ENGINE_TEST_TOKEN") {
		t.Fatalf("the receipt does not name the optional secret the run went without: %q", got)
	}
	if got := env.CapabilitiesEnforced["secrets"]; !strings.Contains(got, "except the optional ones, which a run starts without, written empty into the secrets file when unresolved: ENGINE_TEST_TOKEN") {
		t.Fatalf("the enforcement line does not say the secret is optional: %q", got)
	}
}

func TestASuppliedOptionalSecretIsNotAbsent(t *testing.T) {
	p, f := fixtureEdited(t, nil, func(m *manifest.Manifest) {
		m.Capabilities.OptionalSecrets = []string{"ENGINE_TEST_TOKEN"}
	})
	happyFake(f)
	env, err := Run(context.Background(), f, p)
	if err != nil || env.Status != "ok" {
		t.Fatalf("run: %v", err)
	}
	if got, has := env.CapabilitiesEnforced["secrets.absent"]; has {
		t.Fatalf("a supplied optional secret is named absent: %q", got)
	}
}

// TestARequiredSecretStillRefusesBesideAnOptionalOne is runner plan §R4
// for every secret not marked optional: exit 11, before docker, naming the
// required secret and not the optional one.
func TestARequiredSecretStillRefusesBesideAnOptionalOne(t *testing.T) {
	p, f := fixtureEdited(t, nil, func(m *manifest.Manifest) {
		m.Capabilities.Secrets = append(m.Capabilities.Secrets, "ENGINE_REQUIRED_TOKEN")
		m.Capabilities.OptionalSecrets = []string{"ENGINE_TEST_TOKEN"}
	})
	_ = os.Unsetenv("ENGINE_TEST_TOKEN")
	_ = os.Unsetenv("ENGINE_REQUIRED_TOKEN")
	env, err := Run(context.Background(), f, p)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Code != ExitPolicy {
		t.Fatalf("want exit %d, got %v", ExitPolicy, err)
	}
	wantReason(t, env, err, &receipt.Reason{Code: receipt.ReasonSecretMissing, Names: []string{"ENGINE_REQUIRED_TOKEN"}})
	if strings.Contains(env.Error, "ENGINE_TEST_TOKEN") {
		t.Fatalf("the refusal names the optional secret: %s", env.Error)
	}
	for _, op := range f.Ops() {
		if strings.HasPrefix(op, "Run") || strings.HasPrefix(op, "Network") {
			t.Fatalf("docker touched before the secret check: %v", f.Ops())
		}
	}
}

// TestAnAbsentOptionalSecretIsWrittenEmpty: the gateway sets a variable the
// catalog names to a placeholder of its own when the secrets file lacks its
// key (webSecrets), so the absent secret is written as an empty line, for
// the release's own and for a declared server's.
func TestAnAbsentOptionalSecretIsWrittenEmpty(t *testing.T) {
	p, _ := fixtureEdited(t, nil, func(m *manifest.Manifest) {
		m.Capabilities.OptionalSecrets = []string{"ENGINE_TEST_TOKEN", "SERVER_TOKEN"}
	})
	p.Plan.MCPServers = []spec.MCPServerSpec{{Name: "srv", Secrets: []string{"SERVER_TOKEN"}}}
	_ = os.Unsetenv("ENGINE_TEST_TOKEN")
	_ = os.Unsetenv("SERVER_TOKEN")
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := writeSecretsFile(p, path); err != nil {
		t.Fatalf("writeSecretsFile: %v", err)
	}
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		compile.SecretKey("ENGINE_TEST_TOKEN") + "=\n",
		compile.ServerSecretKey("srv", "SERVER_TOKEN") + "=\n",
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("secrets file lacks the empty line %q:\n%s", want, b)
		}
	}
	values, err := secretValues(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := absentSecrets(p.Plan, values); strings.Join(got, ",") != "ENGINE_TEST_TOKEN,SERVER_TOKEN" {
		t.Fatalf("absentSecrets = %v", got)
	}
}

// TestTheSystemPromptNamesWhatTheRunWentWithout: the compiled System Prompt
// carries spec.PHSecretsAbsent, so the plan is the same bytes whichever
// secrets a run holds, and the rendered prompt names the ones it went
// without, or "none".
func TestTheSystemPromptNamesWhatTheRunWentWithout(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   bool
		wants string
	}{
		{"absent", false, "Not given to this run: ENGINE_TEST_TOKEN."},
		{"given", true, "Not given to this run: none."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := fixtureEdited(t, nil, func(m *manifest.Manifest) {
				m.Capabilities.OptionalSecrets = []string{"ENGINE_TEST_TOKEN"}
			})
			if !tc.set {
				_ = os.Unsetenv("ENGINE_TEST_TOKEN")
			}
			values, err := secretValues(p)
			if err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(t.TempDir(), "cfg")
			rd := &runDirs{root: filepath.Dir(cfg), cfg: cfg, secretsAbsent: absentSecrets(p.Plan, values)}
			if err := renderPlanFiles(p.Plan, p.PlanDir, rd, nativePath, "harness"); err != nil {
				t.Fatal(err)
			}
			rendered := renderedText(t, filepath.Join(cfg, "harness"))
			if !strings.Contains(rendered, "Some declared secrets are optional, and a run may go without them: ENGINE_TEST_TOKEN.") || !strings.Contains(rendered, tc.wants) {
				t.Fatalf("the rendered System Prompt lacks %q:\n%s", tc.wants, rendered)
			}
			if strings.Contains(rendered, spec.PHSecretsAbsent) {
				t.Fatalf("the placeholder survived the render")
			}
		})
	}
}

// TestARunsSystemPromptNamesTheSecretItWentWithout is the same through a
// whole Run, so the order is proved too: the absent names are known before
// the harness files are rendered.
func TestARunsSystemPromptNamesTheSecretItWentWithout(t *testing.T) {
	p, f := fixtureEdited(t, nil, func(m *manifest.Manifest) {
		m.Capabilities.OptionalSecrets = []string{"ENGINE_TEST_TOKEN"}
	})
	happyFake(f)
	p.KeepRunDir = true
	_ = os.Unsetenv("ENGINE_TEST_TOKEN")
	if env, err := Run(context.Background(), f, p); err != nil || env.Status != "ok" {
		t.Fatalf("run: %v", err)
	}
	rendered := renderedText(t, filepath.Join(p.RunRoot, "runs", "cafe0000", "cfg", "harness"))
	if !strings.Contains(rendered, "Not given to this run: ENGINE_TEST_TOKEN.") || strings.Contains(rendered, "@@RUN_") {
		t.Fatalf("the run's System Prompt:\n%s", rendered)
	}
}

// TestReceiptsRedactASuppliedOptionalSecret runs the receipts' redaction
// over a secret marked optional and supplied: it is masked like a required
// one (plan invariant 4).
func TestReceiptsRedactASuppliedOptionalSecret(t *testing.T) {
	p, f := fixtureEdited(t, nil, func(m *manifest.Manifest) {
		m.Capabilities.OptionalSecrets = []string{"ENGINE_TEST_TOKEN"}
	})
	happyFake(f)
	assertReceiptsRedact(t, p, f)
}

// TestAPlanWithoutOptionalSecretsIsUnchanged is plan invariant 2: a
// manifest that marks no secret optional compiles to a plan with no
// optionalSecrets, no placeholder and no optional sentence, so it is the
// bytes it was before DR-70 (the compile package's golden files pin those).
func TestAPlanWithoutOptionalSecretsIsUnchanged(t *testing.T) {
	p, _ := fixture(t)
	raw, err := json.Marshal(p.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "optionalSecrets") {
		t.Fatalf("the plan carries optionalSecrets: %s", raw)
	}
	text := renderedText(t, filepath.Join(p.PlanDir, "harness"))
	if strings.Contains(text, spec.PHSecretsAbsent) || strings.Contains(text, "declared secrets are optional") {
		t.Fatalf("a plan without optional secrets has the optional sentence:\n%s", text)
	}
}

// TestASuppliedOptionalSecretIsScopedLikeARequiredOne is plan invariant 4:
// marking a secret optional changes nothing about where its value goes —
// never into a container's env or argv.
func TestASuppliedOptionalSecretIsScopedLikeARequiredOne(t *testing.T) {
	p, f := fixtureEdited(t, nil, func(m *manifest.Manifest) {
		m.Capabilities.OptionalSecrets = []string{"ENGINE_TEST_TOKEN"}
	})
	happyFake(f)
	_ = os.Unsetenv("ENGINE_TEST_TOKEN")
	p.SecretOverrides = map[string]string{"ENGINE_TEST_TOKEN": "optional-from-profile"}
	if env, err := Run(context.Background(), f, p); err != nil || env.Status != "ok" {
		t.Fatalf("run: %v", err)
	}
	for _, c := range f.Calls {
		for k, v := range c.Run.Env {
			if strings.Contains(v, "optional-from-profile") {
				t.Errorf("an optional secret leaked into %s env %s", c.Run.Name, k)
			}
		}
		for _, a := range c.Run.Argv {
			if strings.Contains(a, "optional-from-profile") {
				t.Errorf("an optional secret leaked into %s argv", c.Run.Name)
			}
		}
	}
}

func renderedText(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			data, rerr := os.ReadFile(path) //nolint:gosec // the test's own render
			if rerr == nil {
				b.Write(data)
			}
		}
		return nil
	})
	if b.Len() == 0 {
		t.Fatalf("nothing rendered under %s", dir)
	}
	return b.String()
}

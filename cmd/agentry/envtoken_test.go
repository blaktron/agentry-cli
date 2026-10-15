package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	saProduction = "https://secureagentry.net"
	saDev        = "https://dev.secureagentry.net"
	selfHosted   = "https://agentry.example.com"
	unrelated    = "https://elsewhere.example.org"
)

// bakeRegistry stands in for a build that bakes reg in as its default.
func bakeRegistry(t *testing.T, reg string) {
	t.Helper()
	was := defaultRegistryURL
	defaultRegistryURL = reg
	t.Cleanup(func() { defaultRegistryURL = was })
}

// captureEnvNotes collects what credentialFor says about tokens it does
// not send, with the said-once memory reset for the test.
func captureEnvNotes(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	was := envNotes
	envNotes = &buf
	envNotesSaid.Clear()
	t.Cleanup(func() { envNotes = was; envNotesSaid.Clear() })
	return &buf
}

// storeFileToken records a config-file sign-in for reg.
func storeFileToken(t *testing.T, reg, token string) {
	t.Helper()
	cfg, err := loadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.upsertRegistry(registryEntry{URL: reg, TokenRef: tokenRefFile, Token: token})
	if err := saveCLIConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestSecureAgentryTokenGoesOnlyToTheBuildsSecureAgentryRegistry(t *testing.T) {
	isolateConfig(t)
	bakeRegistry(t, saProduction)
	t.Setenv(envRegistry, selfHosted)
	t.Setenv(envSecureAgentryToken, " agy_sa \n")
	notes := captureEnvNotes(t)

	for _, target := range []string{saProduction, saProduction + "/", "https://SecureAgentry.net:443", saProduction + "/api/v1/x/runs/rel"} {
		if got, err := credentialFor(target); err != nil || got != "agy_sa" {
			t.Errorf("credentialFor(%s) = %q, %v; want the SecureAgentry token", target, got, err)
		}
	}
	// The other SecureAgentry environment, the self-hosted registry, an
	// unrelated host and a downgrade to plain HTTP get no environment token.
	for _, target := range []string{saDev, selfHosted, unrelated, "http://secureagentry.net", "https://secureagentry.net:8443"} {
		if got, err := credentialFor(target); err != nil || got != "" {
			t.Errorf("credentialFor(%s) = %q, %v; want nothing sent", target, got, err)
		}
	}
	said := notes.String()
	if strings.Contains(said, "agy_sa") {
		t.Fatalf("a note carries the token: %q", said)
	}
	for _, want := range []string{
		envSecureAgentryToken + " is not sent to " + saDev + ": it authenticates only to " + saProduction,
		envSecureAgentryToken + " is not sent to " + unrelated,
		envSecureAgentryToken + " is not sent to " + selfHosted,
	} {
		if !strings.Contains(said, want) {
			t.Errorf("notes %q lack %q", said, want)
		}
	}
	if strings.Count(said, envSecureAgentryToken+" is not sent to "+saDev) != 1 {
		t.Error("a note is said more than once")
	}
}

func TestSecureAgentryTokenIsSentNowhereByABuildForNoSecureAgentryRegistry(t *testing.T) {
	isolateConfig(t)
	bakeRegistry(t, "http://localhost:8080")
	t.Setenv(envSecureAgentryToken, "agy_sa")
	notes := captureEnvNotes(t)
	for _, target := range []string{saProduction, saDev, "http://localhost:8080", unrelated} {
		if got, err := credentialFor(target); err != nil || got != "" {
			t.Errorf("credentialFor(%s) = %q, %v; want nothing sent", target, got, err)
		}
	}
	if !strings.Contains(notes.String(), "this build is for no SecureAgentry registry (its default registry is http://localhost:8080)") {
		t.Fatalf("notes = %q", notes.String())
	}
}

func TestAgentryTokenGoesOnlyToTheConfiguredSelfHostedRegistry(t *testing.T) {
	for _, how := range []string{"AGENTRY_REGISTRY", "defaultRegistry"} {
		t.Run(how, func(t *testing.T) {
			isolateConfig(t)
			bakeRegistry(t, saProduction)
			if how == "AGENTRY_REGISTRY" {
				t.Setenv(envRegistry, selfHosted+"/")
			} else if err := saveCLIConfig(cliConfig{DefaultRegistry: selfHosted}); err != nil {
				t.Fatal(err)
			}
			t.Setenv(envToken, "agy_self")
			notes := captureEnvNotes(t)
			if got, err := credentialFor(selfHosted); err != nil || got != "agy_self" {
				t.Fatalf("credentialFor(self-hosted) = %q, %v", got, err)
			}
			// Never to either SecureAgentry host, even named by --registry,
			// and never to an unrelated one.
			for _, target := range []string{saProduction, saDev, unrelated, "http://agentry.example.com"} {
				if got, err := credentialFor(target); err != nil || got != "" {
					t.Errorf("credentialFor(%s) = %q, %v; want nothing sent", target, got, err)
				}
			}
			said := notes.String()
			if !strings.Contains(said, envToken+" is not sent to "+saProduction+": it authenticates only to "+selfHosted) || strings.Contains(said, "agy_self") {
				t.Fatalf("notes = %q", said)
			}
		})
	}
}

func TestAgentryTokenIsRefusedWithNoSelfHostedRegistry(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"nothing configured": func(t *testing.T) {},
		"AGENTRY_REGISTRY names SecureAgentry": func(t *testing.T) {
			t.Setenv(envRegistry, saDev)
			// A self-hosted defaultRegistry does not stand in for it.
			if err := saveCLIConfig(cliConfig{DefaultRegistry: selfHosted}); err != nil {
				t.Fatal(err)
			}
		},
		"defaultRegistry is SecureAgentry": func(t *testing.T) {
			if err := saveCLIConfig(cliConfig{DefaultRegistry: saProduction}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			isolateConfig(t)
			bakeRegistry(t, saProduction)
			setup(t)
			t.Setenv(envToken, "agy_self")
			storeFileToken(t, saProduction, "agy_stored")
			for _, target := range []string{saProduction, selfHosted, unrelated} {
				got, err := credentialFor(target)
				if !errors.Is(err, errNoSelfHostedRegistry) || got != "" {
					t.Fatalf("credentialFor(%s) = %q, %v; want the refusal", target, got, err)
				}
				if !strings.Contains(err.Error(), envSecureAgentryToken) || strings.Contains(err.Error(), "agy_self") {
					t.Fatalf("refusal = %q", err)
				}
			}
		})
	}
}

// A token that does not apply leaves the target's stored sign-in in force,
// as if the variable were unset.
func TestAnEnvironmentTokenThatDoesNotApplyLeavesTheStoredSignIn(t *testing.T) {
	isolateConfig(t)
	bakeRegistry(t, saProduction)
	t.Setenv(envRegistry, selfHosted)
	t.Setenv(envToken, "agy_self")
	t.Setenv(envSecureAgentryToken, "agy_sa")
	captureEnvNotes(t)
	storeFileToken(t, saDev, "agy_dev_stored")
	storeFileToken(t, selfHosted, "agy_self_stored")
	for target, want := range map[string]string{
		saDev:        "agy_dev_stored", // neither token is for it
		saProduction: "agy_sa",
		selfHosted:   "agy_self", // its environment token is used in place of the stored one
		unrelated:    "",
	} {
		if got, err := credentialFor(target); err != nil || got != want {
			t.Errorf("credentialFor(%s) = %q, %v; want %q", target, got, err, want)
		}
	}
}

func TestIsSecureAgentry(t *testing.T) {
	for reg, want := range map[string]bool{
		saProduction:                         true,
		saDev + "/":                          true,
		"https://Docs.SecureAgentry.net.":    true,
		"http://secureagentry.net:8080":      true,
		"https://secureagentry.net.evil.com": false,
		"https://notsecureagentry.net":       false,
		"https://evil.com/secureagentry.net": false,
		"https://secureagentry.net@evil.com": false,
		"https://secureagentry.net..":        true,
		"https://ｓｅｃｕｒｅａｇｅｎｔｒｙ.net":          true, // fullwidth letters map to ASCII
		"https://dev.secureagentry。net":      true, // an ideographic full stop is a dot
		"https://xn--secureagentry-.net":     false,
		"https://bücher.example":             false,
		selfHosted:                           false,
		"not a url":                          false,
	} {
		if got := isSecureAgentry(reg); got != want {
			t.Errorf("isSecureAgentry(%q) = %v, want %v", reg, got, want)
		}
	}
}

func TestRegistryURLTakesAgentryRegistryBeforeTheConfigFile(t *testing.T) {
	isolateConfig(t)
	bakeRegistry(t, saProduction)
	was := flags.registry
	t.Cleanup(func() { flags.registry = was })
	flags.registry = ""
	if err := saveCLIConfig(cliConfig{DefaultRegistry: unrelated}); err != nil {
		t.Fatal(err)
	}
	if got := registryURL(); got != unrelated {
		t.Fatalf("registryURL() = %q, want the config file's", got)
	}
	t.Setenv(envRegistry, selfHosted+"/")
	if got := registryURL(); got != selfHosted {
		t.Fatalf("registryURL() = %q, want AGENTRY_REGISTRY's", got)
	}
	flags.registry = saDev
	if got := registryURL(); got != saDev {
		t.Fatalf("registryURL() = %q, want --registry's", got)
	}
}

func TestNoCredentialErrorNamesTheVariableForTheTarget(t *testing.T) {
	isolateConfig(t)
	bakeRegistry(t, saProduction)
	for reg, want := range map[string]string{
		saProduction: "set " + envSecureAgentryToken,
		selfHosted:   "set " + envToken,
		saDev:        "run `agentry login`",
	} {
		if got := noCredentialError(reg).Error(); !strings.Contains(got, want) {
			t.Errorf("noCredentialError(%s) = %q, want %q", reg, got, want)
		}
	}
	if got := noCredentialError(saDev).Error(); strings.Contains(got, "_TOKEN") {
		t.Errorf("noCredentialError(%s) = %q names a token that may not go there", saDev, got)
	}
}

// mustCredential is credentialFor for a test that expects no refusal.
func mustCredential(t *testing.T, reg string) string {
	t.Helper()
	token, err := credentialFor(reg)
	if err != nil {
		t.Fatalf("credentialFor(%s): %v", reg, err)
	}
	return token
}

// A stale AGENTRY_TOKEN is refused even beside a SECUREAGENTRY_TOKEN that
// would apply: the person set a token that goes nowhere, and is told.
func TestAgentryTokenIsRefusedBesideASecureAgentryToken(t *testing.T) {
	isolateConfig(t)
	bakeRegistry(t, saProduction)
	t.Setenv(envToken, "agy_self")
	t.Setenv(envSecureAgentryToken, "agy_sa")
	if got, err := credentialFor(saProduction); !errors.Is(err, errNoSelfHostedRegistry) || got != "" {
		t.Fatalf("credentialFor = %q, %v; want the refusal", got, err)
	}
}

// A config file that cannot be read is said, not taken for "nothing
// configured".
func TestAgentryTokenWithAnUnreadableConfigSaysSo(t *testing.T) {
	dir := isolateConfig(t)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("defaultRegistry: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envToken, "agy_self")
	_, err := credentialFor(selfHosted)
	if err == nil || errors.Is(err, errNoSelfHostedRegistry) || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("credentialFor = %v; want the config error", err)
	}
}

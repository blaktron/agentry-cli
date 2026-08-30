package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// isolateConfig points the CLI config at a temp dir and disables the OS
// keychain so tests never touch real credentials.
func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(envConfigDir, dir)
	t.Setenv(envNoKeyring, "1")
	t.Setenv(envToken, "")
	return dir
}

func TestConfigRoundTripAndPermissions(t *testing.T) {
	dir := isolateConfig(t)

	in := cliConfig{
		DefaultRegistry: "http://localhost:8080",
		Registries: []registryEntry{
			{URL: "http://localhost:8080", TokenRef: tokenRefKeyring},
			{URL: "https://registry.example", TokenRef: tokenRefFile, Token: "agy_filetoken"},
		},
	}
	if err := saveCLIConfig(in); err != nil {
		t.Fatal(err)
	}

	got, err := loadCLIConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultRegistry != in.DefaultRegistry || len(got.Registries) != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.Registries[1].Token != "agy_filetoken" || got.Registries[1].TokenRef != tokenRefFile {
		t.Errorf("file token entry corrupted: %+v", got.Registries[1])
	}

	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("config perms = %o, want 600 (may carry a PAT)", perm)
		}
	}
}

func TestLoadCLIConfigMissingFile(t *testing.T) {
	isolateConfig(t)
	cfg, err := loadCLIConfig()
	if err != nil {
		t.Fatalf("missing config must load as zero value: %v", err)
	}
	if cfg.DefaultRegistry != "" || len(cfg.Registries) != 0 {
		t.Errorf("unexpected non-zero config: %+v", cfg)
	}
}

func TestUpsertAndFindRegistry(t *testing.T) {
	var cfg cliConfig
	cfg.upsertRegistry(registryEntry{URL: "http://a.example/", TokenRef: tokenRefKeyring})
	cfg.upsertRegistry(registryEntry{URL: "http://b.example", TokenRef: tokenRefFile, Token: "t1"})
	if len(cfg.Registries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(cfg.Registries))
	}

	// Same URL (modulo trailing slash) replaces in place.
	cfg.upsertRegistry(registryEntry{URL: "http://a.example", TokenRef: tokenRefFile, Token: "t2"})
	if len(cfg.Registries) != 2 {
		t.Fatalf("upsert duplicated an entry: %+v", cfg.Registries)
	}
	e := cfg.findRegistry("http://a.example/")
	if e == nil || e.Token != "t2" || e.TokenRef != tokenRefFile {
		t.Errorf("find after upsert = %+v", e)
	}
	if cfg.findRegistry("http://missing.example") != nil {
		t.Error("find on unknown URL must be nil")
	}
}

func TestCredentialForEnvOverrideWins(t *testing.T) {
	isolateConfig(t)
	t.Setenv(envToken, "  agy_envtoken \n")
	if got := credentialFor("http://localhost:8080"); got != "agy_envtoken" {
		t.Errorf("credentialFor = %q, want trimmed env token", got)
	}
}

func TestCredentialForFileToken(t *testing.T) {
	isolateConfig(t)
	cfg := cliConfig{Registries: []registryEntry{
		{URL: "http://localhost:9999", TokenRef: tokenRefFile, Token: "agy_stored"},
	}}
	if err := saveCLIConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := credentialFor("http://localhost:9999/"); got != "agy_stored" {
		t.Errorf("credentialFor = %q, want config-file token", got)
	}
	if got := credentialFor("http://unknown.example"); got != "" {
		t.Errorf("credentialFor unknown registry = %q, want empty (keyring disabled)", got)
	}
}

func TestRegistryHost(t *testing.T) {
	host, err := registryHost("http://127.0.0.1:8080")
	if err != nil || host != "127.0.0.1:8080" {
		t.Errorf("registryHost = %q, %v", host, err)
	}
	if _, err := registryHost("::not a url"); err == nil {
		t.Error("invalid URL must error")
	}
}

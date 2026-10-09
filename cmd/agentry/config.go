// CLI configuration and credential storage (§11): PATs live in the OS
// keychain via go-keyring, with a chmod-600 config-file fallback for
// headless machines; ~/.config/agentry/config.yaml records the default
// registry and per-registry token references.
package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"

	"github.com/blaktron/agentry-cli/pkg/runner/modelkey"
	"gopkg.in/yaml.v3"

	"github.com/blaktron/agentry-cli/pkg/configdir"
)

// Environment variables understood by the CLI.
const (
	// envToken overrides all stored credentials (CI/scripting escape hatch,
	// used by scripts/acceptance/m3-cli.sh).
	envToken = "AGENTRY_TOKEN"
	// envConfigDir relocates the config directory (default ~/.config/agentry).
	envConfigDir = configdir.EnvVar
	// envNoKeyring forces the config-file credential fallback even when an
	// OS keychain is present (headless/CI use).
	envNoKeyring = modelkey.EnvNoKeyring
	// envRulesDir points `agentry validate` at an S4 rule bundle directory.
	envRulesDir = "AGENTRY_RULES_DIR"
)

// keyringService is the go-keyring service name; the account key is the
// registry host, so multiple registries coexist.
const keyringService = modelkey.Service

// Token reference kinds recorded per registry entry.
const (
	tokenRefKeyring = "keyring"
	tokenRefFile    = "file"
	// A local sign-out suppresses the legacy host-keyed keyring fallback.
	tokenRefNone = "none"
)

// registryEntry is one configured registry.
type registryEntry struct {
	// URL is the registry base URL (no trailing slash).
	URL string `yaml:"url"`
	// TokenRef says where the PAT lives: tokenRefKeyring or tokenRefFile.
	TokenRef string `yaml:"tokenRef,omitempty"`
	// Token holds the PAT only when TokenRef == tokenRefFile.
	Token string `yaml:"token,omitempty"`
}

// cliConfig is ~/.config/agentry/config.yaml.
type cliConfig struct {
	// DefaultRegistry is used when --registry is not passed.
	DefaultRegistry string `yaml:"defaultRegistry,omitempty"`
	// Registries lists known registries and their credential references.
	Registries []registryEntry `yaml:"registries,omitempty"`
	// Report holds this computer's report settings (agentry-cli#731).
	Report reportConfig `yaml:"report,omitempty"`
}

// reportConfig is config.yaml's report block.
type reportConfig struct {
	// Theme is the built-in report theme a run takes when no flag and no
	// signed-in account chooses a design (plan D21).
	Theme string `yaml:"theme,omitempty"`
	// Design is a file holding a report design document (theme document
	// version 2), taken as Theme is; a relative path is the config
	// directory's (report-designs.md D12, agentry-cli#784). At most one of
	// the two is set.
	Design string `yaml:"design,omitempty"`
}

// configDir returns the CLI config directory, honoring envConfigDir.
// Resolution lives in pkg/configdir so pkg/installed and pkg/trust share it.
func configDir() (string, error) {
	return configdir.Dir()
}

// configPath returns the config file path.
func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// loadCLIConfig reads the config file; a missing file yields a zero config.
func loadCLIConfig() (cliConfig, error) {
	var cfg cliConfig
	path, err := configPath()
	if err != nil {
		return cfg, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // fixed, user-owned config path
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// saveCLIConfig writes the config file with 0600 permissions (it may carry
// a PAT when the keyring is unavailable).
func saveCLIConfig(cfg cliConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// The file may predate this write with wider permissions; tighten.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// findRegistry returns the entry matching regURL (trailing slashes ignored),
// or nil.
func (c *cliConfig) findRegistry(regURL string) *registryEntry {
	want := strings.TrimRight(regURL, "/")
	for i := range c.Registries {
		if strings.TrimRight(c.Registries[i].URL, "/") == want {
			return &c.Registries[i]
		}
	}
	return nil
}

// upsertRegistry replaces the entry with e.URL or appends a new one.
func (c *cliConfig) upsertRegistry(e registryEntry) {
	e.URL = strings.TrimRight(e.URL, "/")
	if cur := c.findRegistry(e.URL); cur != nil {
		*cur = e
		return
	}
	c.Registries = append(c.Registries, e)
}

// registryHost extracts the host (the keyring account key) from a registry URL.
func registryHost(regURL string) (string, error) {
	u, err := url.Parse(regURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid registry URL %q", regURL)
	}
	return u.Host, nil
}

// keyringDisabled reports whether the OS keychain must not be touched.
func keyringDisabled() bool {
	return os.Getenv(envNoKeyring) != ""
}

// credentialFor resolves the PAT for a registry: AGENTRY_TOKEN, then a
// config-file token, then the OS keychain (a signed-out schedule's sealed
// keys in its place). Empty means anonymous.
func credentialFor(regURL string) string {
	if t := strings.TrimSpace(os.Getenv(envToken)); t != "" {
		return t
	}
	if cfg, err := loadCLIConfig(); err == nil {
		if e := cfg.findRegistry(regURL); e != nil {
			if e.TokenRef == tokenRefNone {
				return ""
			}
			if e.TokenRef == tokenRefFile && e.Token != "" {
				return e.Token
			}
		}
	}
	host, err := registryHost(regURL)
	if err != nil {
		return ""
	}
	// A signed-out schedule's fire reads the token sealed for it, and
	// never the keychain (agentry-cli#749).
	if token, sealed, _ := modelkey.SealedAccount(host); sealed {
		return token
	}
	if keyringDisabled() {
		return ""
	}
	token, err := keyring.Get(keyringService, host)
	if err != nil {
		return ""
	}
	return token
}

// keyringSet stores a PAT in the OS keychain under the registry host.
// It fails when the keychain is unavailable or disabled via AGENTRY_NO_KEYRING;
// callers fall back to the config file.
func keyringSet(host, token string) error {
	if keyringDisabled() {
		return fmt.Errorf("keyring disabled via %s", envNoKeyring)
	}
	if err := keyring.Set(keyringService, host, token); err != nil {
		return fmt.Errorf("keyring store: %w", err)
	}
	return nil
}

func keyringDelete(host string) error {
	if keyringDisabled() {
		return fmt.Errorf("keyring disabled via %s", envNoKeyring)
	}
	err := keyring.Delete(keyringService, host)
	if err == keyring.ErrNotFound {
		return nil
	}
	return err
}

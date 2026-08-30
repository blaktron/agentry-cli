// Package configdir resolves the agentry client configuration directory
// (default ~/.config/agentry, relocated by AGENTRY_CONFIG_DIR). Every file
// any agentry client keeps on disk — config.yaml, installed.yaml, pinned
// signing keys, cached rule bundles — lives under this one root, so a single
// environment variable relocates all of it (container volumes, tests).
//
// It is a separate package so pkg/installed and pkg/trust can share the
// resolution with the CLI without importing the CLI's command wiring.
package configdir

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvVar relocates the config directory (default ~/.config/agentry).
const EnvVar = "AGENTRY_CONFIG_DIR"

// Dir returns the agentry config directory, honoring EnvVar.
func Dir() (string, error) {
	if d := os.Getenv(EnvVar); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "agentry"), nil
}

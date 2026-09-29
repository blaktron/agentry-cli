// Package profile stores a QuickAgent's MCP profile (RUNNER-PLAN-2 §P2):
// the persistent, credentialed half of its gateway configuration, keyed by
// signature — ns/name@version plus the archive digest, the same identity
// the install cache uses. A profile holds the application keys and
// identities the declared contract needs so a later run of the same
// release "just uses the authenticated resources"; whether it survives the
// run is the operator's keep/remove choice.
//
// Values live only here (dir 0700, file 0600) and in the per-run gateway
// secrets file the engine deletes at teardown; they never enter plans,
// receipts, or process listings — the v1 invariants hold unchanged.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Version is the profile schema major.
const Version = "1"

// Profile is one QuickAgent signature's stored configuration.
type Profile struct {
	// ProfileVersion pins the schema ("1").
	ProfileVersion string `json:"profileVersion"`
	// Ref is ns/name@version; Digest is the verified archive digest. A
	// different digest is a different trust decision and gets a fresh
	// guided setup (P2 §P8).
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	// Keep records the operator's completion choice: true → the profile
	// survives the run; false → the run deletes it on completion.
	Keep bool `json:"keep"`
	// CreatedAt / LastUsedAt are RFC3339 audit stamps.
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt,omitempty"`
	// Secrets maps declared env NAMES to the values guided setup
	// collected. A name absent here falls back to the policy secret
	// source at run time.
	Secrets map[string]string `json:"secrets,omitempty"`
}

// Dir is the profile directory for one signature under the runner home.
func Dir(home, ns, name, version, digestSuffix string) string {
	return filepath.Join(home, "profiles", ns, name, version+"-"+digestSuffix)
}

// Load reads a profile; (nil, nil) when none exists — absence is the
// normal first-run state, not an error.
func Load(dir string) (*Profile, error) {
	b, err := os.ReadFile(filepath.Join(dir, "profile.json")) //nolint:gosec // our profile layout
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read profile: %w", err)
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, "profile.json"), err)
	}
	if p.ProfileVersion != Version {
		return nil, fmt.Errorf("profile version %q not supported (want %q) — remove %s and reconfigure", p.ProfileVersion, Version, dir)
	}
	return &p, nil
}

// Save writes the profile with owner-only permissions.
func (p *Profile) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write profile: %w", err)
	}
	return nil
}

// Touch stamps LastUsedAt and persists.
func (p *Profile) Touch(dir string) error {
	p.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	return p.Save(dir)
}

// Remove deletes a profile directory; absence is not an error.
func Remove(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove profile: %w", err)
	}
	return nil
}

// New builds a fresh profile for a signature.
func New(ref, digest string, keep bool, secrets map[string]string) *Profile {
	return &Profile{
		ProfileVersion: Version,
		Ref:            ref,
		Digest:         digest,
		Keep:           keep,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Secrets:        secrets,
	}
}

// Entry is one stored profile plus its location, for listings.
type Entry struct {
	Dir     string
	Profile *Profile
}

// List walks every stored profile under the runner home, sorted by ref.
// Unreadable entries are skipped with their error attached to a nil
// Profile so `agentry profiles list` can show, not hide, a broken one.
func List(home string) ([]Entry, error) {
	root := filepath.Join(home, "profiles")
	var out []Entry
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() || info.Name() != "profile.json" {
			return nil
		}
		dir := filepath.Dir(path)
		p, perr := Load(dir)
		if perr != nil || p == nil {
			out = append(out, Entry{Dir: dir})
			return nil
		}
		out = append(out, Entry{Dir: dir, Profile: p})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list profiles: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := "", ""
		if out[i].Profile != nil {
			ri = out[i].Profile.Ref
		}
		if out[j].Profile != nil {
			rj = out[j].Profile.Ref
		}
		if ri == rj {
			return out[i].Dir < out[j].Dir
		}
		return ri < rj
	})
	return out, nil
}

// SecretNames lists the stored secret names, sorted — for display; values
// are never printed by any surface.
func (p *Profile) SecretNames() []string {
	names := make([]string, 0, len(p.Secrets))
	for n := range p.Secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// MatchesRef reports whether the profile belongs to ns/name or
// ns/name@version (the `agentry profiles remove` selector).
func (p *Profile) MatchesRef(sel string) bool {
	if p == nil {
		return false
	}
	if sel == p.Ref {
		return true
	}
	base, _, hasVer := strings.Cut(p.Ref, "@")
	return !strings.Contains(sel, "@") && hasVer && sel == base
}

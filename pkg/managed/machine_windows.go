//go:build windows

package managed

import (
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// platformMachineSource reads the policy key's values Registry,
// RequireOrg and Settings (a JSON object), as the JSON the file holds on
// other systems.
func platformMachineSource() ([]byte, string, bool, error) {
	where := `HKLM\` + machineKey
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, where, false, nil
	}
	if err != nil {
		return nil, where, false, err
	}
	defer k.Close()
	var f struct {
		Registry   string          `json:"registry,omitempty"`
		RequireOrg string          `json:"requireOrg,omitempty"`
		Settings   json.RawMessage `json:"settings,omitempty"`
	}
	read := func(name string) (string, error) {
		v, _, err := k.GetStringValue(name)
		if errors.Is(err, registry.ErrNotExist) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("value %s: %w", name, err)
		}
		return v, nil
	}
	if f.Registry, err = read("Registry"); err != nil {
		return nil, where, true, err
	}
	if f.RequireOrg, err = read("RequireOrg"); err != nil {
		return nil, where, true, err
	}
	settings, err := read("Settings")
	if err != nil {
		return nil, where, true, err
	}
	if settings != "" {
		f.Settings = json.RawMessage(settings)
	}
	if f.Registry == "" && f.RequireOrg == "" && settings == "" {
		return nil, where, false, nil
	}
	raw, err := json.Marshal(f)
	return raw, where, true, err
}

//go:build darwin

package managed

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// machinePath is where a configuration profile's managed preferences for
// Agentry land: the domain net.secureagentry.agentry.
var machinePath = "/Library/Managed Preferences/net.secureagentry.agentry.plist"

// platformMachineSource reads the managed preferences, held to an
// administrator's ownership first, as JSON through plutil, which reads
// both of a property list's formats.
func platformMachineSource() ([]byte, string, bool, error) {
	_, where, found, err := readAdminFile(machinePath)
	if err != nil || !found {
		return nil, where, found, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "--", machinePath).Output()
	if err != nil {
		return nil, where, true, fmt.Errorf("plutil could not read it: %w", err)
	}
	// A profile's payload may carry keys of its own beside these three:
	// only Agentry's are kept.
	var all map[string]json.RawMessage
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, where, true, fmt.Errorf("it is not a dictionary: %w", err)
	}
	ours := map[string]json.RawMessage{}
	for _, k := range []string{"registry", "requireOrg", "settings"} {
		if v, ok := all[k]; ok {
			ours[k] = v
		}
	}
	raw, err := json.Marshal(ours)
	return raw, where, true, err
}

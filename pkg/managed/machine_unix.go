//go:build !windows && !darwin

package managed

// machinePath is where the machine settings file is on this system.
var machinePath = "/etc/agentry/managed.json"

// platformMachineSource reads the machine settings file, held to an
// administrator's ownership first.
func platformMachineSource() ([]byte, string, bool, error) {
	return readAdminFile(machinePath)
}

//go:build !windows

package schedule

// systemZone is empty where /etc/localtime names the zone.
func systemZone() string { return "" }

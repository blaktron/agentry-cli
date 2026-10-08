package schedule

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalZone is this computer's IANA zone name, the default for a new
// schedule: $TZ, else the zone /etc/localtime links to, else UTC. Go's
// time.Local is named "Local", which no other system can read back.
func LocalZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if name := zoneFromPath(target); name != "" {
			return name
		}
	}
	if n := time.Local.String(); n != "Local" && n != "" {
		return n
	}
	return "UTC"
}

// zoneFromPath reads a zone name from a zoneinfo path
// (/usr/share/zoneinfo/Europe/London → Europe/London).
func zoneFromPath(p string) string {
	const marker = "zoneinfo/"
	i := strings.LastIndex(p, marker)
	if i < 0 {
		return ""
	}
	name := p[i+len(marker):]
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}

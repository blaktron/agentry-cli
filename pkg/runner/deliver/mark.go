package deliver

import (
	"fmt"
	"time"
)

// zoneIdentifier is the content of the Zone.Identifier stream Windows keeps
// beside a file that came from outside the computer (ZoneId 3 is the
// Internet): the Mark-of-the-Web, which makes Explorer and Office treat the
// file as a download.
const zoneIdentifier = "[ZoneTransfer]\r\nZoneId=3\r\n"

// quarantineValue is the value of the com.apple.quarantine attribute macOS
// keeps on a downloaded file: flags, the time in hexadecimal seconds, and
// the downloading program.
func quarantineValue(now time.Time) string {
	return fmt.Sprintf("0081;%x;Agentry;", now.Unix())
}

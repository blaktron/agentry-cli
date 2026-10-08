package schedule

import (
	"unsafe"

	"golang.org/x/sys/windows"

	// A Windows computer has no zoneinfo of its own, and
	// time.LoadLocation finds Go's only where Go is installed: the zones
	// are carried in the binary.
	_ "time/tzdata"
)

var (
	icu             = windows.NewLazySystemDLL("icu.dll")
	ucalDefaultZone = icu.NewProc("ucal_getDefaultTimeZone")
)

// icuZoneUnits bounds the zone name ICU writes, in UTF-16 units.
const icuZoneUnits = 128

// systemZone is the IANA name of the zone Windows is set to, as the
// system's own ICU (icu.dll, Windows 10 1903 and later) maps it; empty
// when it cannot say.
func systemZone() string {
	if ucalDefaultZone.Find() != nil {
		return ""
	}
	buf := make([]uint16, icuZoneUnits)
	var status int32 // UErrorCode: above zero is a failure
	n, _, _ := ucalDefaultZone.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&status)))
	if status > 0 || int32(n) <= 0 || int(n) > len(buf) {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

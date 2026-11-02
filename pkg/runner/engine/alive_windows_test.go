package engine

import "golang.org/x/sys/windows"

// stillActive is the exit code GetExitCodeProcess reports for a process
// that is still running (STILL_ACTIVE).
const stillActive = 259

// alive says whether a process with pid exists and has not exited.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // a pid from this test
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == stillActive
}

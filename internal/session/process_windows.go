//go:build windows

package session

import (
	"golang.org/x/sys/windows"
)

// processAlive reports whether pid is a live process. Windows: an
// OpenProcess handle with query access; a missing process fails with
// ERROR_INVALID_PARAMETER (the handle is then closed by the deferred Close).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	// STILL_ACTIVE (259) means the process has not exited.
	return exitCode == 259
}

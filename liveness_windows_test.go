//go:build windows

package processkit

import (
	"os"

	"golang.org/x/sys/windows"
)

const stillActive = 259 // STILL_ACTIVE: a process that has not yet exited.

// processAlive reports whether a process with the given pid currently exists and
// has not exited.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false // can't open it → treat as gone
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// selfSig exists so TestMain compiles on Windows; the Signalled test that uses it
// is Unix-only (Windows has no signal abstraction).
func selfSig() { os.Exit(42) }

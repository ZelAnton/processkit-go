//go:build windows

package processkit

import "syscall"

// Windows error codes that signal a transient spawn failure: another handle holds
// the executable open in an incompatible sharing mode, or a byte-range lock blocks
// the read. Both typically clear on a brief retry.
const (
	errorSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION
	errorLockViolation    syscall.Errno = 33 // ERROR_LOCK_VIOLATION
)

// isTransientErrno reports whether a Windows error code is a transient spawn
// failure worth retrying.
func isTransientErrno(e syscall.Errno) bool {
	switch e {
	case errorSharingViolation, errorLockViolation:
		return true
	default:
		return false
	}
}

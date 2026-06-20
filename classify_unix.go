//go:build unix

package processkit

import "syscall"

// isTransientErrno reports whether a Unix errno is a transient spawn failure:
// EINTR (interrupted), EAGAIN/EWOULDBLOCK (would block / resource temporarily
// unavailable), ETXTBSY (text file busy — a freshly-written executable), or EBUSY.
func isTransientErrno(e syscall.Errno) bool {
	switch e {
	case syscall.EINTR, syscall.EAGAIN, syscall.ETXTBSY, syscall.EBUSY:
		return true
	default:
		return false
	}
}

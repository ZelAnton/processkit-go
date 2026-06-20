//go:build unix

package processkit

import "syscall"

// number maps a curated [Signal] to this platform's signal number (so SIGUSR1 is
// correct on Linux and macOS, where the numbers differ), or returns the raw number
// for a [RawSignal].
func (s Signal) number() int {
	switch s.kind {
	case sigTerm:
		return int(syscall.SIGTERM)
	case sigKill:
		return int(syscall.SIGKILL)
	case sigInt:
		return int(syscall.SIGINT)
	case sigHup:
		return int(syscall.SIGHUP)
	case sigQuit:
		return int(syscall.SIGQUIT)
	case sigUsr1:
		return int(syscall.SIGUSR1)
	case sigUsr2:
		return int(syscall.SIGUSR2)
	default:
		return s.raw
	}
}

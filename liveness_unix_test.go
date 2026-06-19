//go:build unix

package processkit

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with the given pid currently exists.
// Signal 0 probes without delivering; ESRCH means gone, EPERM means it exists
// but isn't ours.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// selfSig (helper mode "selfsig") kills the current process with SIGKILL, so the
// run captures a Signalled outcome.
func selfSig() { _ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL) }

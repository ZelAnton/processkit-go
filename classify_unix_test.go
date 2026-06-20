//go:build unix

package processkit

import (
	"syscall"
	"testing"
)

func TestIsTransient_UnixErrnos(t *testing.T) {
	transient := []syscall.Errno{syscall.EINTR, syscall.EAGAIN, syscall.ETXTBSY, syscall.EBUSY}
	for _, e := range transient {
		// Wrap in a *StartError, the way a real spawn failure surfaces, to confirm
		// IsTransient unwraps to the errno.
		if !IsTransient(&StartError{Program: "x", Err: e}) {
			t.Errorf("IsTransient(%v) = false, want true", e)
		}
	}
	if IsTransient(&StartError{Program: "x", Err: syscall.EACCES}) {
		t.Error("IsTransient(EACCES) = true, want false (a permission error is not transient)")
	}
}

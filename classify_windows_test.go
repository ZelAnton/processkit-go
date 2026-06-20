//go:build windows

package processkit

import (
	"syscall"
	"testing"
)

func TestIsTransient_WindowsErrnos(t *testing.T) {
	for _, e := range []syscall.Errno{errorSharingViolation, errorLockViolation} {
		if !IsTransient(&StartError{Program: "x", Err: e}) {
			t.Errorf("IsTransient(%v) = false, want true", e)
		}
	}
	if IsTransient(&StartError{Program: "x", Err: syscall.Errno(5)}) { // ERROR_ACCESS_DENIED
		t.Error("IsTransient(ERROR_ACCESS_DENIED) = true, want false")
	}
}

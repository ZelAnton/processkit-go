package processkit

import (
	"errors"
	"testing"
)

func TestIsTransient_NonErrnoIsFalse(t *testing.T) {
	cases := []error{
		nil,
		errors.New("plain error"),
		&ExitError{Program: "x", Outcome: exited(1)},
		&NotFoundError{Program: "x"},
	}
	for _, err := range cases {
		if IsTransient(err) {
			t.Errorf("IsTransient(%v) = true, want false", err)
		}
	}
}

package processkit

import (
	"errors"
	"syscall"
)

// IsTransient reports whether err is a transient, low-level failure worth
// retrying — an interrupted or would-block syscall, a busy executable or file,
// or (on Windows) a sharing/lock violation. It is meant as a [Cmd.WithRetry]
// classifier for spawn hiccups.
//
// It deliberately does NOT treat a non-zero exit code or a timeout as transient:
// those are domain-specific (a "git exited 128" is not generically retryable).
// Classify those yourself, e.g. errors.Is(err, [ErrTimeout]) to retry timeouts.
func IsTransient(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return isTransientErrno(errno)
	}
	return false
}

package processkit

import (
	"context"
	"time"
)

// sleepCtx sleeps for d, returning true if it elapsed or false if ctx ended first.
// A non-positive d does not sleep; it just reports whether ctx is still live. It is
// the ctx-aware backoff used by the supervisor and by [Cmd.WithRetry].
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// clock is the internal time seam, so deadline / grace / backoff logic is
// testable without real sleeps. The package uses realClock; tests inject a fake.
type clock interface {
	Now() time.Time
	// NewTimer returns a one-shot timer channel and a stop function, so callers
	// can release the timer when they finish early.
	NewTimer(d time.Duration) (<-chan time.Time, func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

package processkit

import "time"

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

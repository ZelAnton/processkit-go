package processkit

import (
	"context"
	"errors"
	"net"
	"time"
)

// readinessPoll is the cadence at which [RunningProcess.WaitForPort] and
// [RunningProcess.WaitFor] re-check readiness.
const readinessPoll = 50 * time.Millisecond

// connectAttemptCap bounds a single [RunningProcess.WaitForPort] dial, so one
// stalled connect can't hang the whole probe.
const connectAttemptCap = time.Second

// errStreamNotEnabled is returned by WaitForLine when the process was not started
// with [StreamLines], so there is no line channel to watch.
var errStreamNotEnabled = errors.New("processkit: WaitForLine requires Group.Start with StreamLines()")

// WaitForLine waits until a line of the process's merged output (stdout or stderr)
// satisfies match, and returns that line. It consumes the [RunningProcess.Lines]
// channel up to and including the matched line, so probe first, then continue
// ranging Lines for the rest — don't read Lines concurrently with this.
//
// The process must have been started with [StreamLines], and you must not range
// [RunningProcess.Lines] concurrently with this (they would steal lines from each
// other). WaitForLine does not kill the process: on the within deadline it returns
// a [*NotReadyError] (the process keeps running); if the output stream ends before
// a match (the process exited) it returns a [*NotReadyError] promptly; if ctx is
// cancelled it returns ctx's bare error. A zero or negative within still checks the
// already-buffered lines once.
func (p *RunningProcess) WaitForLine(ctx context.Context, match func(string) bool, within time.Duration) (string, error) {
	if p.lines == nil {
		return "", errStreamNotEnabled
	}
	notReady := func() (string, error) {
		return "", &NotReadyError{Program: p.program, Probe: "line", Timeout: within}
	}
	timer := time.NewTimer(nonNegative(within))
	defer timer.Stop()

	// First, scan the lines already buffered at entry — bounded by that count so a
	// flood can't starve the deadline — so a match that is ready right now (even at
	// a zero deadline) is seen before the timer.
	for buffered := len(p.lines); buffered > 0; buffered-- {
		select {
		case line, ok := <-p.lines:
			if !ok {
				return notReady()
			}
			if match(line.Text) {
				return line.Text, nil
			}
		default:
			buffered = 1 // channel drained; the loop's post-decrement ends it
		}
	}

	// Then wait for new lines until the deadline, honoring cancellation. Go's select
	// chooses fairly among ready cases, so a flood of non-matching lines cannot
	// starve the timer or the context.
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				return notReady()
			}
			if match(line.Text) {
				return line.Text, nil
			}
		case <-timer.C:
			return notReady()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// WaitForPort waits until a TCP connection to addr (a "host:port" address, IPv4 or
// bracketed IPv6) succeeds. It dials repeatedly on a ~50ms cadence, trying
// immediately first, and drops each successful connection at once. It tests the
// address, not this specific process — if the process dies and something else binds
// the port, the probe reports ready (use [RunningProcess.WaitFor] with an identity
// check if that matters). Like the other probes it does not kill the process: on
// the within deadline it returns a [*NotReadyError] carrying the last dial error;
// if the process exits first it returns a [*NotReadyError] promptly; if ctx is
// cancelled it returns ctx's bare error. The deadline may be overrun by up to one
// connect attempt (≤1s).
func (p *RunningProcess) WaitForPort(ctx context.Context, addr string, within time.Duration) error {
	return p.pollUntil(ctx, "port", within, func(c context.Context) (bool, error) {
		d := net.Dialer{Timeout: connectAttemptCap}
		conn, err := d.DialContext(c, "tcp", addr)
		if err != nil {
			return false, err
		}
		_ = conn.Close()
		return true, nil
	})
}

// WaitFor waits until check returns true, re-invoking it on a ~50ms cadence (and
// immediately first). check should be a cheap, non-blocking readiness test — an
// HTTP /health request, a file existing, a database accepting connections. It is
// passed ctx so it can honour cancellation. Like the other probes it does not kill
// the process: on the within deadline it returns a [*NotReadyError]; if the
// process exits first it returns one promptly; if ctx is cancelled it returns ctx's
// bare error.
func (p *RunningProcess) WaitFor(ctx context.Context, check func(context.Context) bool, within time.Duration) error {
	return p.pollUntil(ctx, "predicate", within, func(c context.Context) (bool, error) {
		return check(c), nil
	})
}

// pollUntil drives the poll-based probes (port, predicate): it tries check
// immediately, then on the readiness cadence until check passes, the process
// exits, the deadline elapses, or ctx is cancelled.
func (p *RunningProcess) pollUntil(ctx context.Context, probe string, within time.Duration, check func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(nonNegative(within))
	var lastErr error
	for {
		if ready, err := check(ctx); ready {
			return nil
		} else {
			lastErr = err
		}
		if p.exited() {
			return &NotReadyError{Program: p.program, Probe: probe, Timeout: within, Cause: lastErr}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &NotReadyError{Program: p.program, Probe: probe, Timeout: within, Cause: lastErr}
		}
		sleep := readinessPoll
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// exited reports whether the process has been reaped, without blocking.
func (p *RunningProcess) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// nonNegative clamps a negative duration to zero (a deadline already in the past).
func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

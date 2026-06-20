package processkit

import (
	"log/slog"
	"time"
)

// runLog is processkit's structured-logging seam. It wraps an optional
// *slog.Logger — nil is the default and makes every method a no-op — and exposes
// one method per logged event.
//
// Centralising the catalogue here keeps the security rule structural, not
// incidental: no method ever accepts a command's arguments, environment, working
// directory, or stream content, so those can never reach a log record. Only the
// program *identity*, pid, mechanism, outcome, durations, counts, and an
// already-sanitized error string are ever logged. (Matching the Rust crate's
// "never logs argv or environment values" discipline.)
//
// All events use Debug for ordinary lifecycle and Warn for anomalies — there is no
// Info or Error tier (a failure is returned as a typed error for the caller to log
// as it sees fit). slog has no Trace level, so the one Rust trace event (adopt) is
// Debug here.
type runLog struct{ l *slog.Logger }

// errAttr renders an error as a safe attribute. processkit's error strings are
// bounded and sanitized (control/bidi stripped, env/PATH redacted), so the
// Error() text is safe to log; a nil error yields an empty string.
func errAttr(err error) slog.Attr {
	if err == nil {
		return slog.String("error", "")
	}
	return slog.String("error", err.Error())
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

// --- lifecycle (Debug) ---

func (r runLog) spawned(program string, pid int, mech Mechanism) {
	if r.l == nil {
		return
	}
	r.l.Debug("child spawned",
		slog.String("program", program), slog.Int("pid", pid), slog.String("mechanism", mech.String()))
}

func (r runLog) exited(program string, outcome Outcome, elapsed time.Duration) {
	if r.l == nil {
		return
	}
	r.l.Debug("process exited",
		slog.String("program", program), slog.String("outcome", outcome.String()), slog.Int64("elapsed_ms", ms(elapsed)))
}

// reapFailed is the terminal event for the rare case where a started process's
// Wait fails without yielding an exit status, so it has no clean outcome — every
// started process still emits exactly one terminal event.
func (r runLog) reapFailed(program string, cause error) {
	if r.l == nil {
		return
	}
	r.l.Warn("process wait failed; no exit status", slog.String("program", program), errAttr(cause))
}

func (r runLog) cancelled(program string) {
	if r.l == nil {
		return
	}
	r.l.Debug("cancellation fired; killing the tree", slog.String("program", program))
}

func (r runLog) retrying(program string, attempt, maxAttempts int, backoff time.Duration, cause error) {
	if r.l == nil {
		return
	}
	r.l.Debug("retrying after a retryable failure",
		slog.String("program", program), slog.Int("attempt", attempt), slog.Int("max_attempts", maxAttempts),
		slog.Int64("backoff_ms", ms(backoff)), errAttr(cause))
}

func (r runLog) adopted(pid int, mech Mechanism) {
	if r.l == nil {
		return
	}
	r.l.Debug("adopted an externally spawned child",
		slog.Int("pid", pid), slog.String("mechanism", mech.String()))
}

func (r runLog) terminating(mech Mechanism) {
	if r.l == nil {
		return
	}
	r.l.Debug("terminating every process in the group", slog.String("mechanism", mech.String()))
}

func (r runLog) shuttingDown(mech Mechanism, grace time.Duration) {
	if r.l == nil {
		return
	}
	r.l.Debug("graceful shutdown: signal, grace, then kill",
		slog.String("mechanism", mech.String()), slog.Int64("grace_ms", ms(grace)))
}

func (r runLog) supervisorRestart(program string, restart int, backoff time.Duration) {
	if r.l == nil {
		return
	}
	// backoff_ms matches the retry event's field name — both are "wait this long
	// before re-running" (Cmd.WithRetry backoff / Supervisor.WithBackoff).
	r.l.Debug("supervisor restarting child",
		slog.String("program", program), slog.Int("restart", restart), slog.Int64("backoff_ms", ms(backoff)))
}

func (r runLog) pipelineStarted(program string, stages int) {
	if r.l == nil {
		return
	}
	r.l.Debug("pipeline started", slog.String("program", program), slog.Int("stages", stages))
}

func (r runLog) pipelineFinished(program string, outcome Outcome, elapsed time.Duration) {
	if r.l == nil {
		return
	}
	r.l.Debug("pipeline finished",
		slog.String("program", program), slog.String("outcome", outcome.String()), slog.Int64("elapsed_ms", ms(elapsed)))
}

// --- anomalies (Warn) ---

func (r runLog) timedOut(program string, timeout time.Duration) {
	if r.l == nil {
		return
	}
	r.l.Warn("timeout elapsed; killing the tree",
		slog.String("program", program), slog.Int64("timeout_ms", ms(timeout)))
}

func (r runLog) supervisorStorm(program string, pause time.Duration) {
	if r.l == nil {
		return
	}
	r.l.Warn("supervisor failure storm; pausing restarts",
		slog.String("program", program), slog.Int64("pause_ms", ms(pause)))
}

package processkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Sentinel errors for use with errors.Is. The data-carrying error types below
// match the relevant sentinels through their Is methods.
var (
	// ErrCancelled means the run was abandoned via its context. Unlike a timeout
	// (which is captured in the [Result]), a cancellation is always an error and
	// carries no output. It wins over a co-occurring timeout.
	ErrCancelled = errors.New("processkit: run cancelled")

	// ErrTimeout means the run exceeded its deadline and was killed. A timed-out
	// [*ExitError] matches this via errors.Is.
	ErrTimeout = errors.New("processkit: run timed out")

	// ErrUnsupported means the operation is not available on this platform (e.g.
	// arbitrary signals on Windows, uid/gid drop off Unix). Never a silent skip.
	ErrUnsupported = errors.New("processkit: operation not supported on this platform")

	// ErrNotReady means a readiness probe did not pass within its deadline (or can
	// no longer pass). Distinct from ErrTimeout, which is the run's own deadline.
	ErrNotReady = errors.New("processkit: readiness probe did not pass")

	// ErrResourceLimit means a requested whole-tree resource cap could not be
	// enforced — never a silently-unbounded group.
	ErrResourceLimit = errors.New("processkit: resource limit could not be enforced")

	// ErrNotFound means the program could not be found. A [*NotFoundError] matches
	// this via errors.Is.
	ErrNotFound = errors.New("processkit: program not found")

	// ErrTooFewStages means a [Pipeline] was run with fewer than two stages. A
	// pipeline needs at least two commands to chain.
	ErrTooFewStages = errors.New("processkit: a pipeline needs at least two stages")
)

// ExitError reports a run that completed but was not a success — a non-zero exit
// code, a signal kill (Unix), or a timeout. It carries the captured output so the
// caller can diagnose the failure. Match it with errors.As; a timed-out ExitError
// additionally matches errors.Is(err, [ErrTimeout]).
type ExitError struct {
	Program   string
	Outcome   Outcome
	Stdout    string
	Stderr    string
	Mechanism Mechanism
}

// Error renders a safe, bounded summary. Captured streams are previewed (not
// dumped in full) and sanitized so child-controlled bytes can't inject terminal
// escapes or bidi overrides (Trojan-Source, CVE-2021-42574).
func (e *ExitError) Error() string {
	var b strings.Builder
	b.WriteString("processkit: ")
	b.WriteString(quoteProgram(e.Program))
	switch {
	case e.Outcome.TimedOut():
		b.WriteString(" timed out")
	default:
		if s, ok := e.Outcome.Signal(); ok {
			fmt.Fprintf(&b, " killed by signal %d", s)
		} else if c, ok := e.Outcome.Code(); ok {
			fmt.Fprintf(&b, " exited with code %d", c)
		} else {
			b.WriteString(" terminated abnormally")
		}
	}
	if d := diagnostic(e.Stderr, e.Stdout); d != "" {
		b.WriteString(": ")
		b.WriteString(d)
	}
	return b.String()
}

// Is reports a match for ErrTimeout when this exit was a timeout, so
// errors.Is(err, ErrTimeout) works on a timed-out ExitError.
func (e *ExitError) Is(target error) bool {
	return target == ErrTimeout && e.Outcome.TimedOut()
}

// CancelError reports that a run was ended by the caller's context — either
// cancelled or its deadline elapsed. It carries no captured output (the run was
// abandoned). Matches errors.Is(err, [ErrCancelled]); Cause is the underlying
// context error, so errors.Is(err, context.Canceled) / context.DeadlineExceeded
// also work. (A run's *own* [Cmd.WithTimeout] deadline is captured in the
// [Result] instead — see [Outcome.TimedOut].)
type CancelError struct {
	Program string
	Cause   error // context.Canceled or context.DeadlineExceeded
}

// Error renders the cancellation, distinguishing a cancelled context from an
// elapsed parent deadline (the Is/Unwrap match against [ErrCancelled] and the
// underlying context error is unaffected either way).
func (e *CancelError) Error() string {
	reason := "cancelled"
	if errors.Is(e.Cause, context.DeadlineExceeded) {
		reason = "context deadline exceeded"
	}
	return fmt.Sprintf("processkit: %s %s", quoteProgram(e.Program), reason)
}

// Is matches the ErrCancelled sentinel.
func (e *CancelError) Is(target error) bool { return target == ErrCancelled }

// Unwrap exposes the underlying context error to errors.Is / errors.As.
func (e *CancelError) Unwrap() error { return e.Cause }

// NotFoundError reports that a program could not be resolved. Searched holds the
// PATH directories that were checked, when known. Matches errors.Is(err, [ErrNotFound]).
type NotFoundError struct {
	Program  string
	Searched []string
}

// Error renders the failure, naming how many PATH directories were searched (not
// their contents, to avoid leaking the environment).
func (e *NotFoundError) Error() string {
	if len(e.Searched) > 0 {
		return fmt.Sprintf("processkit: %s not found on PATH (searched %d director%s)",
			quoteProgram(e.Program), len(e.Searched), plural(len(e.Searched), "y", "ies"))
	}
	return fmt.Sprintf("processkit: %s not found", quoteProgram(e.Program))
}

// Is matches the ErrNotFound sentinel.
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// StartError reports a spawn failure that is not a not-found (e.g. a permission
// error, a bad working directory). It wraps the underlying cause.
type StartError struct {
	Program string
	Err     error
}

// Error renders the spawn failure.
func (e *StartError) Error() string {
	return fmt.Sprintf("processkit: failed to start %s: %v", quoteProgram(e.Program), e.Err)
}

// Unwrap exposes the underlying OS error to errors.Is / errors.As.
func (e *StartError) Unwrap() error { return e.Err }

// NotReadyError reports that a readiness probe ([RunningProcess.WaitForLine],
// [RunningProcess.WaitForPort], [RunningProcess.WaitFor]) did not pass — the line
// never appeared, the port never accepted, the predicate never held, or the
// process exited before becoming ready. Matches errors.Is(err, [ErrNotReady]).
//
// It is distinct from [ErrTimeout]: a probe deadline is the caller's own
// readiness budget, not the run's [Cmd.WithTimeout], and a failed probe does NOT
// kill the process — the caller decides what happens next.
type NotReadyError struct {
	Program string        // the process that did not become ready
	Probe   string        // which probe: "line", "port", or "predicate"
	Timeout time.Duration // the probe deadline that elapsed
	Cause   error         // the last underlying failure (e.g. the last dial error), if any
}

// Error renders the readiness failure.
func (e *NotReadyError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("processkit: %s not ready (%s probe) after %s: %v",
			quoteProgram(e.Program), e.Probe, e.Timeout, e.Cause)
	}
	return fmt.Sprintf("processkit: %s not ready (%s probe) after %s",
		quoteProgram(e.Program), e.Probe, e.Timeout)
}

// Is matches the ErrNotReady sentinel.
func (e *NotReadyError) Is(target error) bool { return target == ErrNotReady }

// Unwrap exposes the last underlying failure (if any) to errors.Is / errors.As.
func (e *NotReadyError) Unwrap() error { return e.Cause }

// --- safe rendering helpers (shared by the error types) ---

// previewLimit bounds how many bytes of a captured stream appear in an error
// string — enough to diagnose, not enough to dump a multi-megabyte capture.
const previewLimit = 200

// diagnostic picks the most useful captured stream (stderr, falling back to
// stdout) and returns a bounded, sanitized preview, or "" if both are empty.
func diagnostic(stderr, stdout string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		s = strings.TrimSpace(stdout)
	}
	if s == "" {
		return ""
	}
	return sanitize(preview(s))
}

// preview truncates s to previewLimit bytes on a rune boundary, marking a cut.
func preview(s string) string {
	if len(s) <= previewLimit {
		return s
	}
	cut := previewLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// sanitize escapes control characters (except \n and \t) and Unicode bidi /
// directional-override controls, defusing terminal-injection and Trojan-Source
// attacks (CVE-2021-42574) from child-controlled output.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// quoteProgram renders a program name for an error, sanitized and backtick-quoted.
func quoteProgram(p string) string {
	if p == "" {
		return "`<command>`"
	}
	return "`" + sanitize(p) + "`"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

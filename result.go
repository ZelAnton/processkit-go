package processkit

import (
	"fmt"
	"strings"
	"time"
)

// Outcome describes how a process ended: a normal exit with a code, a kill by a
// signal (Unix only), or a timeout. Inspect it via the accessors; a missing exit
// code is reported as (0, false), never a fabricated -1 sentinel.
//
// Outcomes are normally produced by running a command; for a fake [ProcessRunner]
// or a custom runner, build one with [Exited], [Signalled], or [TimedOut] (and a
// whole [Result] with [NewResult]). The zero value is exited(0) — build outcomes
// with the constructors rather than relying on a bare Outcome{}.
type Outcome struct {
	kind      outcomeKind
	code      int  // valid when kind == outcomeExited
	signal    int  // valid when kind == outcomeSignalled && hasSignal
	hasSignal bool // whether a signal number is known
}

type outcomeKind uint8

const (
	outcomeExited outcomeKind = iota
	outcomeSignalled
	outcomeTimedOut
)

// exited builds an Outcome for a normal termination with the given exit code.
func exited(code int) Outcome { return Outcome{kind: outcomeExited, code: code} }

// signalled builds an Outcome for a signal kill; sig is the signal number, or nil
// when unknown. Unix-only — a Windows kill surfaces as exited, never signalled
// (report the platform truth, don't fabricate a signal from an NTSTATUS).
func signalled(sig *int) Outcome {
	o := Outcome{kind: outcomeSignalled}
	if sig != nil {
		o.signal, o.hasSignal = *sig, true
	}
	return o
}

// timedOut builds an Outcome for a run killed by its own deadline.
func timedOut() Outcome { return Outcome{kind: outcomeTimedOut} }

// Exited builds an [Outcome] for a normal termination with the given exit code.
// It is a construction seam for fake [ProcessRunner]s (see [NewResult]); real
// runs produce outcomes themselves.
func Exited(code int) Outcome { return exited(code) }

// Signalled builds an [Outcome] for a Unix signal kill with the given signal
// number — for fakes modelling a signal kill. (A real Windows kill is reported as
// exited, never signalled.)
func Signalled(signal int) Outcome { return signalled(&signal) }

// TimedOut builds an [Outcome] for a run killed by its own deadline — for fakes.
func TimedOut() Outcome { return timedOut() }

// Code returns the exit code and true for a normal exit; (0, false) for a signal
// kill or a timeout.
func (o Outcome) Code() (int, bool) {
	if o.kind == outcomeExited {
		return o.code, true
	}
	return 0, false
}

// Signal returns the signal number and true when the process was killed by a
// known signal (Unix only); (0, false) otherwise.
func (o Outcome) Signal() (int, bool) {
	if o.kind == outcomeSignalled && o.hasSignal {
		return o.signal, true
	}
	return 0, false
}

// TimedOut reports whether the run was killed by its own timeout.
func (o Outcome) TimedOut() bool { return o.kind == outcomeTimedOut }

// String renders the outcome, e.g. "exited(0)", "signalled(9)", "timedOut".
func (o Outcome) String() string {
	switch o.kind {
	case outcomeExited:
		return fmt.Sprintf("exited(%d)", o.code)
	case outcomeSignalled:
		if o.hasSignal {
			return fmt.Sprintf("signalled(%d)", o.signal)
		}
		return "signalled"
	case outcomeTimedOut:
		return "timedOut"
	default:
		return "unknown"
	}
}

// Result is the captured result of a run. A non-zero exit is *data* here, not an
// error: it is reported in the Outcome and only turned into an error by the
// success-requiring verbs ([Cmd.Run], [Cmd.ExitCode], [Cmd.Probe]) or by
// [Result.Err].
type Result struct {
	program   string
	args      []string
	outcome   Outcome
	stdout    []byte // captured stdout, exactly as produced (use Stdout for \n-normalized text)
	stderr    string // captured stderr, normalized to \n
	duration  time.Duration
	okCodes   []int // exit codes treated as success in addition to 0
	mechanism Mechanism
}

// NewResult builds a [Result] for a fake [ProcessRunner] to return from its Output
// method — the construction seam the dependency-injection model needs (a real run
// produces a Result through the verbs; a double or a custom runner builds one
// here). The program, args, and ok-codes are taken from inv; stdout is stored as
// produced and stderr is normalized to \n. The mechanism is [MechanismUnknown] and
// the duration is zero (set neither for a fake). Build the outcome with [Exited],
// [Signalled], or [TimedOut].
func NewResult(inv Invocation, outcome Outcome, stdout, stderr []byte) *Result {
	return &Result{
		program:   inv.Program,
		args:      append([]string(nil), inv.Args...),
		outcome:   outcome,
		stdout:    append([]byte(nil), stdout...),
		stderr:    normalizeNewlines(stderr),
		okCodes:   append([]int(nil), inv.OkCodes...),
		mechanism: MechanismUnknown,
	}
}

// Program returns the program that was run.
func (r *Result) Program() string { return r.program }

// Args returns a copy of the arguments the program was run with.
func (r *Result) Args() []string { return append([]string(nil), r.args...) }

// Outcome returns how the process ended.
func (r *Result) Outcome() Outcome { return r.outcome }

// StdoutBytes returns a copy of the captured stdout, exactly as produced (line
// endings preserved). Use this for binary output; use [Result.Stdout] for text.
func (r *Result) StdoutBytes() []byte { return append([]byte(nil), r.stdout...) }

// Stdout returns the captured stdout as text, with line endings normalized to
// \n. Use [Result.StdoutBytes] for exact bytes.
func (r *Result) Stdout() string { return normalizeNewlines(r.stdout) }

// Stderr returns the captured stderr as text (normalized to \n).
func (r *Result) Stderr() string { return r.stderr }

// Duration returns how long the run took.
func (r *Result) Duration() time.Duration { return r.duration }

// Mechanism returns the containment mechanism that was in effect for the run.
func (r *Result) Mechanism() Mechanism { return r.mechanism }

// Code returns the exit code and true for a normal exit; (0, false) otherwise.
func (r *Result) Code() (int, bool) { return r.outcome.Code() }

// TimedOut reports whether the run was killed by its own timeout.
func (r *Result) TimedOut() bool { return r.outcome.TimedOut() }

// Success reports whether the run succeeded: a clean exit whose code is 0 or in
// the configured OkCodes set.
func (r *Result) Success() bool {
	code, ok := r.outcome.Code()
	if !ok {
		return false
	}
	if code == 0 {
		return true
	}
	for _, ok2 := range r.okCodes {
		if code == ok2 {
			return true
		}
	}
	return false
}

// Err returns nil when the run was a success, otherwise an [*ExitError] carrying
// the captured streams (as sanitized text). The success-requiring verbs report this.
func (r *Result) Err() error {
	if r.Success() {
		return nil
	}
	return r.toExitError()
}

func (r *Result) toExitError() *ExitError {
	return &ExitError{
		Program:   r.program,
		Outcome:   r.outcome,
		Stdout:    normalizeNewlines(r.stdout),
		Stderr:    r.stderr,
		Mechanism: r.mechanism,
	}
}

// normalizeNewlines converts CRLF and lone CR to LF, for text accessors.
func normalizeNewlines(b []byte) string {
	s := string(b)
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

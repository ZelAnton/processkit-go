// Package processkittest provides hermetic test doubles for the processkit
// [processkit.ProcessRunner] seam: a [ScriptedRunner] that answers commands with
// canned replies, a [RecordingRunner] that records the invocations a wrapper makes
// for assertions, and a [RecordReplayRunner] that records real runs to a JSON
// cassette and replays them. They are hand-written, idiomatic Go dependency
// injection — no mock framework — and (except a recorder's first pass) need no real
// subprocess.
//
// Inject one into a command, a [processkit.CliClient], or a typed wrapper built on
// a client:
//
//	scripted := processkittest.NewScriptedRunner().
//		On([]string{"git", "status"}, processkittest.OK("clean")).
//		Fallback(processkittest.Fail(1, "unexpected command"))
//	git := &Git{client: processkit.NewClient("git").WithRunner(scripted)}
package processkittest

import (
	"context"
	"fmt"
	"sync"

	processkit "github.com/ZelAnton/processkit-go"
)

// Reply is a canned outcome a [ScriptedRunner] returns for a matched command.
// Build one with [OK], [Fail], [TimedOut], [Signalled], [Err], or [Pending], and
// attach captured output with [Reply.WithStdout] / [Reply.WithStderr].
type Reply struct {
	outcome processkit.Outcome
	stdout  []byte
	stderr  []byte
	err     error // a spawn/IO failure to return instead of a result
	pending bool  // park until the context is cancelled, then return a cancel error
}

// OK replies with a clean exit (code 0) and the given stdout.
func OK(stdout string) Reply {
	return Reply{outcome: processkit.Exited(0), stdout: []byte(stdout)}
}

// Fail replies with a non-zero exit code and the given stderr. (If the command's
// ok-codes accept code, the run still counts as a success.)
func Fail(code int, stderr string) Reply {
	return Reply{outcome: processkit.Exited(code), stderr: []byte(stderr)}
}

// TimedOut replies as a run killed by its own deadline.
func TimedOut() Reply {
	return Reply{outcome: processkit.TimedOut()}
}

// Signalled replies as a Unix signal kill with the given signal number.
func Signalled(signal int) Reply {
	return Reply{outcome: processkit.Signalled(signal)}
}

// Err replies with a spawn/IO failure — the runner returns err instead of a
// result (for example a [processkit.NotFoundError] to model a missing program).
func Err(err error) Reply {
	return Reply{err: err}
}

// Pending replies by parking the call until the context is cancelled, then
// returning a cancellation error — for exercising real cancellation handling.
func Pending() Reply {
	return Reply{pending: true}
}

// WithStdout attaches stdout to a reply (e.g. output alongside a [Fail]).
func (r Reply) WithStdout(s string) Reply {
	r.stdout = []byte(s)
	return r
}

// WithStderr attaches stderr to a reply.
func (r Reply) WithStderr(s string) Reply {
	r.stderr = []byte(s)
	return r
}

// resolve turns the reply into the runner's return values for invocation inv.
func (r Reply) resolve(ctx context.Context, inv processkit.Invocation) (*processkit.Result, error) {
	switch {
	case r.pending:
		<-ctx.Done()
		return nil, &processkit.CancelError{Program: inv.Program, Cause: ctx.Err()}
	case r.err != nil:
		return nil, r.err
	default:
		return processkit.NewResult(inv, r.outcome, r.stdout, r.stderr), nil
	}
}

// ScriptedRunner is a [processkit.ProcessRunner] that answers commands with canned
// replies. Configure it with the chainable On / OnSequence / When / Fallback
// builders; an invocation that matches no rule and no fallback fails loudly (a
// [processkit.NotFoundError]), so an unexpected command never slips through a test
// silently. It is safe for concurrent use.
type ScriptedRunner struct {
	mu       sync.Mutex
	rules    []*rule
	fallback *Reply
}

type rule struct {
	prefix    []string                         // program-then-args prefix, or nil for a predicate rule
	predicate func(processkit.Invocation) bool // nil for a prefix rule
	replies   []Reply                          // one reply, or a sequence
	cursor    int                              // next reply index for a sequence
}

// NewScriptedRunner creates an empty scripted runner. Add rules with the builders.
func NewScriptedRunner() *ScriptedRunner { return &ScriptedRunner{} }

// On answers a command whose program-then-args start with prefix (e.g.
// []string{"git", "status"} matches `git status …`) with reply, every time.
func (s *ScriptedRunner) On(prefix []string, reply Reply) *ScriptedRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &rule{prefix: append([]string(nil), prefix...), replies: []Reply{reply}})
	return s
}

// OnSequence answers successive matches of prefix with successive replies; once
// the sequence is exhausted the last reply repeats. A sequence drives a retried
// command (each attempt takes the next reply). It panics if replies is empty.
func (s *ScriptedRunner) OnSequence(prefix []string, replies ...Reply) *ScriptedRunner {
	if len(replies) == 0 {
		panic("processkittest: OnSequence needs at least one reply")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &rule{prefix: append([]string(nil), prefix...), replies: append([]Reply(nil), replies...)})
	return s
}

// When answers any command the predicate accepts (it sees the full invocation —
// program, args, dir, env) with reply.
func (s *ScriptedRunner) When(predicate func(processkit.Invocation) bool, reply Reply) *ScriptedRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &rule{predicate: predicate, replies: []Reply{reply}})
	return s
}

// Fallback sets the reply for any command no rule matched (otherwise an unmatched
// command fails with a [processkit.NotFoundError]).
func (s *ScriptedRunner) Fallback(reply Reply) *ScriptedRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallback = &reply
	return s
}

// Output implements [processkit.ProcessRunner].
func (s *ScriptedRunner) Output(ctx context.Context, inv processkit.Invocation) (*processkit.Result, error) {
	s.mu.Lock()
	reply, ok := s.match(inv)
	s.mu.Unlock()
	if !ok {
		return nil, &processkit.NotFoundError{Program: inv.Program}
	}
	return reply.resolve(ctx, inv)
}

// match finds the reply for inv (advancing a sequence cursor); callers hold s.mu.
func (s *ScriptedRunner) match(inv processkit.Invocation) (Reply, bool) {
	for _, ru := range s.rules {
		if !ru.matches(inv) {
			continue
		}
		i := ru.cursor
		if i >= len(ru.replies) {
			i = len(ru.replies) - 1 // exhausted sequence repeats the last reply
		} else {
			ru.cursor++
		}
		return ru.replies[i], true
	}
	if s.fallback != nil {
		return *s.fallback, true
	}
	return Reply{}, false
}

func (ru *rule) matches(inv processkit.Invocation) bool {
	if ru.predicate != nil {
		return ru.predicate(inv)
	}
	full := append([]string{inv.Program}, inv.Args...)
	if len(ru.prefix) > len(full) {
		return false
	}
	for i, p := range ru.prefix {
		if full[i] != p {
			return false
		}
	}
	return true
}

// RecordingRunner is a [processkit.ProcessRunner] that records every invocation it
// is asked to run, then delegates to an inner runner — for asserting which
// commands a wrapper built. It is safe for concurrent use.
type RecordingRunner struct {
	inner processkit.ProcessRunner
	mu    sync.Mutex
	calls []processkit.Invocation
}

// NewRecordingRunner wraps inner, recording each invocation before delegating.
func NewRecordingRunner(inner processkit.ProcessRunner) *RecordingRunner {
	return &RecordingRunner{inner: inner}
}

// Replying returns a recording runner whose inner runner answers every command
// with reply — the common case when you only need to assert on inputs.
func Replying(reply Reply) *RecordingRunner {
	return NewRecordingRunner(NewScriptedRunner().Fallback(reply))
}

// Output implements [processkit.ProcessRunner].
func (r *RecordingRunner) Output(ctx context.Context, inv processkit.Invocation) (*processkit.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, cloneInvocation(inv))
	r.mu.Unlock()
	return r.inner.Output(ctx, inv)
}

// Calls returns a snapshot of the invocations recorded so far, in order.
func (r *RecordingRunner) Calls() []processkit.Invocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]processkit.Invocation(nil), r.calls...)
}

// OnlyCall returns the single recorded invocation, panicking unless exactly one
// command was run — the common assertion for a wrapper method that runs one tool.
func (r *RecordingRunner) OnlyCall() processkit.Invocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 1 {
		panic(fmt.Sprintf("processkittest: expected exactly one call, got %d", len(r.calls)))
	}
	return r.calls[0]
}

// cloneInvocation deep-copies the borrowed invocation slices so a recorded call
// can't be mutated by a later run.
func cloneInvocation(inv processkit.Invocation) processkit.Invocation {
	inv.Args = append([]string(nil), inv.Args...)
	if inv.Env != nil {
		inv.Env = append([]string{}, inv.Env...)
	}
	inv.OkCodes = append([]int(nil), inv.OkCodes...)
	return inv
}

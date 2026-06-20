package processkit

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// scriptedRunner is a ProcessRunner double that returns a fixed sequence of
// (result, error) replies — the hermetic seam for supervision logic. It records
// how many times it ran.
type scriptedRunner struct {
	replies []scriptReply
	calls   int
}

type scriptReply struct {
	res *Result
	err error
}

func (r *scriptedRunner) Output(_ context.Context, _ Invocation) (*Result, error) {
	if r.calls >= len(r.replies) {
		// Exhausting the script is a test bug; surface it loudly rather than hang.
		return nil, errors.New("scriptedRunner: ran out of replies")
	}
	rep := r.replies[r.calls]
	r.calls++
	return rep.res, rep.err
}

func exitReply(code int, okCodes ...int) scriptReply {
	return scriptReply{res: &Result{program: "svc", outcome: exited(code), okCodes: okCodes}}
}
func timeoutReply() scriptReply {
	return scriptReply{res: &Result{program: "svc", outcome: timedOut()}}
}
func errReply(err error) scriptReply { return scriptReply{err: err} }

// superv builds a supervisor over a scripted runner with backoff and jitter off,
// so the logic tests run instantly and deterministically.
func superv(replies ...scriptReply) *Supervisor {
	return Supervise(Command("svc")).
		WithRunner(&scriptedRunner{replies: replies}).
		WithBackoff(0, 1).
		WithJitter(false)
}

func TestSupervisor_OnCrashStopsOnCleanFirstRun(t *testing.T) {
	out, err := superv(exitReply(0)).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 0 || out.Stopped != StoppedPolicySatisfied {
		t.Fatalf("got restarts=%d stopped=%v, want 0/policy-satisfied", out.Restarts, out.Stopped)
	}
	if !out.Final.Success() {
		t.Fatalf("Final = %v, want success", out.Final.Outcome())
	}
}

func TestSupervisor_OnCrashRestartsThenSucceeds(t *testing.T) {
	out, err := superv(exitReply(1), exitReply(1), exitReply(0)).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 2 || out.Stopped != StoppedPolicySatisfied {
		t.Fatalf("got restarts=%d stopped=%v, want 2/policy-satisfied", out.Restarts, out.Stopped)
	}
	if !out.Final.Success() {
		t.Fatalf("Final = %v, want the clean third run", out.Final.Outcome())
	}
}

func TestSupervisor_NeverRunsOnce(t *testing.T) {
	out, err := superv(exitReply(1)).WithRestart(RestartNever).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 0 || out.Stopped != StoppedPolicySatisfied {
		t.Fatalf("got restarts=%d stopped=%v, want 0/policy-satisfied", out.Restarts, out.Stopped)
	}
}

func TestSupervisor_AlwaysUntilMaxRestarts(t *testing.T) {
	out, err := superv(exitReply(0), exitReply(0), exitReply(0), exitReply(0)).
		WithRestart(RestartAlways).WithMaxRestarts(3).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 3 || out.Stopped != StoppedRestartsExhausted {
		t.Fatalf("got restarts=%d stopped=%v, want 3/restarts-exhausted", out.Restarts, out.Stopped)
	}
}

func TestSupervisor_MaxRestartsZeroSingleRun(t *testing.T) {
	out, err := superv(exitReply(1)).WithMaxRestarts(0).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 0 || out.Stopped != StoppedRestartsExhausted {
		t.Fatalf("got restarts=%d stopped=%v, want 0/restarts-exhausted", out.Restarts, out.Stopped)
	}
}

func TestSupervisor_StopWhenBeatsPolicy(t *testing.T) {
	// Under Always (which would restart a clean run), the predicate stops it.
	out, err := superv(exitReply(7)).
		WithRestart(RestartAlways).
		StopWhen(func(r *Result) bool { c, ok := r.Code(); return ok && c == 7 }).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 0 || out.Stopped != StoppedByPredicate {
		t.Fatalf("got restarts=%d stopped=%v, want 0/predicate", out.Restarts, out.Stopped)
	}
}

func TestSupervisor_AcceptedNonZeroExitIsClean(t *testing.T) {
	out, err := superv(exitReply(2, 2)).Run(context.Background()) // ok-code 2
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Stopped != StoppedPolicySatisfied {
		t.Fatalf("stopped = %v, want policy-satisfied (exit 2 is an ok-code)", out.Stopped)
	}
}

func TestSupervisor_TimeoutIsCrash(t *testing.T) {
	out, err := superv(timeoutReply()).WithMaxRestarts(0).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Stopped != StoppedRestartsExhausted || !out.Final.TimedOut() {
		t.Fatalf("got stopped=%v timedOut=%v, want restarts-exhausted/true", out.Stopped, out.Final.TimedOut())
	}
}

func TestSupervisor_SpawnErrorRetriedThenSucceeds(t *testing.T) {
	out, err := superv(errReply(&StartError{Program: "svc", Err: errors.New("boom")}), exitReply(0)).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 1 || out.Stopped != StoppedPolicySatisfied {
		t.Fatalf("got restarts=%d stopped=%v, want 1/policy-satisfied (spawn error retried)", out.Restarts, out.Stopped)
	}
}

func TestSupervisor_SpawnErrorSurfacedUnderNever(t *testing.T) {
	nf := &NotFoundError{Program: "svc"}
	_, err := superv(errReply(nf)).WithRestart(RestartNever).Run(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want the surfaced NotFound error", err)
	}
}

func TestSupervisor_SpawnErrorSurfacedWhenBudgetExhausted(t *testing.T) {
	se := &StartError{Program: "svc", Err: errors.New("boom")}
	_, err := superv(errReply(se), errReply(se)).WithMaxRestarts(1).Run(context.Background())
	var got *StartError
	if !errors.As(err, &got) {
		t.Fatalf("err = %v, want the surfaced *StartError when the budget is spent", err)
	}
}

func TestSupervisor_CancelledIncarnationIsTerminal(t *testing.T) {
	ce := &CancelError{Program: "svc", Cause: context.Canceled}
	_, err := superv(errReply(ce)).WithRestart(RestartAlways).Run(context.Background())
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled (a cancelled incarnation is terminal)", err)
	}
}

func TestSupervisor_CallerCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sup := Supervise(Command("svc")).
		WithRunner(&scriptedRunner{replies: []scriptReply{exitReply(1), exitReply(0)}}).
		WithBackoff(500*time.Millisecond, 1).WithJitter(false)
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()
	_, err := sup.Run(ctx)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled (cancel during backoff)", err)
	}
}

func TestSupervisor_PreCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := superv(exitReply(0)).Run(ctx)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled for an already-cancelled context", err)
	}
}

// TestSupervisor_StormGuardPauses drives a tight crash loop and checks the storm
// guard takes a pause once the decaying score crosses the threshold.
func TestSupervisor_StormGuardPauses(t *testing.T) {
	replies := make([]scriptReply, 9)
	for i := range replies {
		replies[i] = exitReply(1)
	}
	out, err := Supervise(Command("svc")).
		WithRunner(&scriptedRunner{replies: replies}).
		WithBackoff(0, 1).WithJitter(false).
		WithMaxRestarts(8).
		WithStormPause(2 * time.Millisecond). // threshold 5, decay 30s by default
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.StormPauses < 1 {
		t.Fatalf("StormPauses = %d, want at least one (tight crash loop)", out.StormPauses)
	}
}

// --- pure-function tests ---

func TestBackoffDelay(t *testing.T) {
	base := 200 * time.Millisecond
	cap30 := 30 * time.Second
	cases := []struct {
		n      int
		factor float64
		want   time.Duration
	}{
		{0, 2.0, 200 * time.Millisecond},
		{1, 2.0, 400 * time.Millisecond},
		{2, 2.0, 800 * time.Millisecond},
		{100, 2.0, cap30},                // overflow saturates at the cap
		{3, 0.5, 200 * time.Millisecond}, // factor < 1 → constant
		{5, math.NaN(), 200 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := backoffDelay(tc.n, base, tc.factor, cap30); got != tc.want {
			t.Errorf("backoffDelay(%d, factor=%v) = %v, want %v", tc.n, tc.factor, got, tc.want)
		}
	}
	if got := backoffDelay(3, 0, 2.0, cap30); got != 0 {
		t.Errorf("zero base = %v, want 0", got)
	}
}

func TestApplyJitter(t *testing.T) {
	d := time.Second
	if got := applyJitter(d, func() float64 { return 0 }, true); got != d/2 {
		t.Errorf("jitter at 0.0 = %v, want %v", got, d/2)
	}
	hi := applyJitter(d, func() float64 { return 0.999999 }, true)
	if hi <= d || hi >= 3*d/2+time.Millisecond {
		t.Errorf("jitter near 1.0 = %v, want just under 1.5s", hi)
	}
	if got := applyJitter(d, func() float64 { return 0.3 }, false); got != d {
		t.Errorf("disabled jitter = %v, want %v unchanged", got, d)
	}
	if got := applyJitter(0, func() float64 { return 0.3 }, true); got != 0 {
		t.Errorf("zero delay = %v, want 0", got)
	}
}

func TestStormState_Record(t *testing.T) {
	decay := 30 * time.Second
	base := time.Now()
	var st stormState

	if s := st.record(base, decay); s != 1 {
		t.Fatalf("first failure score = %v, want 1", s)
	}
	if s := st.record(base, decay); s != 2 { // dt 0 → no decay, +1
		t.Fatalf("immediate second failure = %v, want 2", s)
	}
	// After one half-life of quiet, the score halves before the +1.
	if s := st.record(base.Add(decay), decay); math.Abs(s-2.0) > 1e-9 { // 2*0.5 + 1
		t.Fatalf("after one half-life = %v, want 2.0", s)
	}
	// Zero decay keeps no history: always 1.
	var z stormState
	z.record(base, 0)
	if s := z.record(base, 0); s != 1 {
		t.Fatalf("zero-decay score = %v, want 1", s)
	}
	// Reset clears history.
	st.reset()
	if s := st.record(base, decay); s != 1 {
		t.Fatalf("post-reset score = %v, want 1", s)
	}
}

// --- real-subprocess tests ---

func TestSupervisor_RealFlappingExhausts(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	out, err := Supervise(Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=1")...)).
		WithMaxRestarts(2).WithBackoff(0, 1).WithJitter(false).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 2 || out.Stopped != StoppedRestartsExhausted {
		t.Fatalf("got restarts=%d stopped=%v, want 2/restarts-exhausted", out.Restarts, out.Stopped)
	}
	if c, ok := out.Final.Code(); !ok || c != 1 {
		t.Fatalf("Final code = %v, want 1", out.Final.Outcome())
	}
}

func TestSupervisor_RealCleanStops(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	out, err := Supervise(Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=0")...)).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Restarts != 0 || out.Stopped != StoppedPolicySatisfied || !out.Final.Success() {
		t.Fatalf("got restarts=%d stopped=%v success=%v, want 0/policy-satisfied/true",
			out.Restarts, out.Stopped, out.Final.Success())
	}
}

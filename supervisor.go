package processkit

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"
)

// RestartPolicy decides when a [Supervisor] re-runs its command.
type RestartPolicy uint8

const (
	// RestartOnCrash restarts only after a crash — a clean run ends supervision
	// ([StoppedPolicySatisfied]). This is the default (the zero value). A crash is
	// any run that is not a success ([Result.Success]): a rejected exit code, a
	// timeout, a signal kill, or a spawn failure. (Exit code 0 is always a success;
	// [Cmd.WithOkCodes] widens which other codes count.)
	RestartOnCrash RestartPolicy = iota
	// RestartAlways restarts after every run, clean or not. It loops until a
	// [Supervisor.StopWhen] predicate or [Supervisor.WithMaxRestarts] stops it.
	RestartAlways
	// RestartNever runs the command exactly once.
	RestartNever
)

// String renders the policy.
func (p RestartPolicy) String() string {
	switch p {
	case RestartAlways:
		return "always"
	case RestartNever:
		return "never"
	default:
		return "on-crash"
	}
}

// wantsRestart reports whether the policy wants another run after a run that did
// (crashed) or did not (clean) crash.
func (p RestartPolicy) wantsRestart(crashed bool) bool {
	switch p {
	case RestartAlways:
		return true
	case RestartNever:
		return false
	default: // RestartOnCrash
		return crashed
	}
}

// StopReason explains why a [Supervisor] concluded.
type StopReason uint8

const (
	// StoppedByPredicate means a [Supervisor.StopWhen] predicate matched a run.
	StoppedByPredicate StopReason = iota
	// StoppedPolicySatisfied means the [RestartPolicy] wanted no further run (a
	// clean run under RestartOnCrash, or any run under RestartNever).
	StoppedPolicySatisfied
	// StoppedRestartsExhausted means the restart budget ([Supervisor.WithMaxRestarts])
	// ran out while the policy still wanted to restart.
	StoppedRestartsExhausted
)

// String renders the stop reason.
func (r StopReason) String() string {
	switch r {
	case StoppedByPredicate:
		return "predicate"
	case StoppedRestartsExhausted:
		return "restarts-exhausted"
	default:
		return "policy-satisfied"
	}
}

// SupervisionOutcome is what a concluded [Supervisor.Run] reports. A non-error
// Run means supervision concluded — inspect Final for the last run's verdict (it
// may be a failure, e.g. when the restart budget ran out).
type SupervisionOutcome struct {
	// Final is the [Result] of the last (terminating) run.
	Final *Result
	// Restarts is the number of re-runs; the first run is not a restart, so
	// Restarts == 2 means three runs total.
	Restarts int
	// Stopped is why supervision ended.
	Stopped StopReason
	// StormPauses is how many failure-storm pauses were taken (0 unless
	// [Supervisor.WithStormPause] is set).
	StormPauses int
}

// Supervisor keeps a command alive: it runs the command, and on a crash re-runs
// it with capped-exponential backoff, optionally guarding against a crash storm,
// until a stop condition is met. It is a value built by chainable WithX methods
// (each returning a new, independent *Supervisor) and run with [Supervisor.Run].
//
// Supervision is sequential and single-flight: the next run never starts until
// the previous one has fully exited and been reaped, so a command's whole tree is
// always torn down before a restart — no overlap, no orphan.
type Supervisor struct {
	cmd    *Cmd
	runner ProcessRunner

	policy      RestartPolicy
	maxRestarts int // < 0 means unlimited
	backoffBase time.Duration
	factor      float64
	maxBackoff  time.Duration
	jitter      bool

	stormPause       time.Duration // 0 disables the storm guard
	failureDecay     time.Duration
	failureThreshold float64

	stopWhen func(*Result) bool
	rng      func() float64 // jitter source in [0,1); nil → time-seeded default
}

// Supervise starts building a supervisor for cmd with sensible defaults:
// RestartOnCrash, unlimited restarts, 200ms base backoff doubling to a 30s cap,
// jitter on, and the failure-storm guard off. The command runs once per
// incarnation; its WithTimeout / WithOkCodes / WithEnv etc. apply each run, but
// its WithRunner and WithRetry do not — set the supervisor's runner with
// [Supervisor.WithRunner], and let the restart policy drive retries.
func Supervise(cmd *Cmd) *Supervisor {
	return &Supervisor{
		cmd:              cmd,
		policy:           RestartOnCrash,
		maxRestarts:      -1,
		backoffBase:      200 * time.Millisecond,
		factor:           2.0,
		maxBackoff:       30 * time.Second,
		jitter:           true,
		failureDecay:     30 * time.Second,
		failureThreshold: 5.0,
	}
}

func (s *Supervisor) clone() *Supervisor {
	cp := *s
	return &cp
}

// WithRestart sets the restart policy (default [RestartOnCrash]).
func (s *Supervisor) WithRestart(policy RestartPolicy) *Supervisor {
	cp := s.clone()
	cp.policy = policy
	return cp
}

// WithMaxRestarts caps the number of restarts over the supervisor's lifetime;
// after n restarts (n+1 runs) it stops with [StoppedRestartsExhausted]. n == 0
// means a single run, and a negative n is treated as 0. The default (don't call
// this) is unlimited.
func (s *Supervisor) WithMaxRestarts(n int) *Supervisor {
	cp := s.clone()
	if n < 0 {
		n = 0
	}
	cp.maxRestarts = n
	return cp
}

// WithBackoff sets the base delay before the first restart and the multiplier
// applied per subsequent restart (delay n = base × factor^n, capped by
// [Supervisor.WithMaxBackoff]). A base of 0 restarts immediately; a factor below
// 1 (or non-finite) is treated as 1 (a constant delay).
func (s *Supervisor) WithBackoff(base time.Duration, factor float64) *Supervisor {
	cp := s.clone()
	cp.backoffBase = base
	cp.factor = factor
	return cp
}

// WithMaxBackoff caps the per-restart backoff delay (default 30s).
func (s *Supervisor) WithMaxBackoff(limit time.Duration) *Supervisor {
	cp := s.clone()
	cp.maxBackoff = limit
	return cp
}

// WithJitter enables or disables ±50% jitter on every backoff and storm pause
// (default on). Disable it for deterministic delays in tests.
func (s *Supervisor) WithJitter(enabled bool) *Supervisor {
	cp := s.clone()
	cp.jitter = enabled
	return cp
}

// WithStormPause turns on the failure-storm guard and sets the pause it takes
// when a crash storm is detected. The guard is off by default; pass 0 to keep it
// off. See [Supervisor.WithFailureDecay] and [Supervisor.WithFailureThreshold].
func (s *Supervisor) WithStormPause(pause time.Duration) *Supervisor {
	cp := s.clone()
	cp.stormPause = pause
	return cp
}

// WithFailureDecay sets the half-life of the failure-storm score (default 30s):
// the score halves every decay of quiet, so failures spaced wider than this never
// build a storm. Only meaningful with [Supervisor.WithStormPause].
func (s *Supervisor) WithFailureDecay(decay time.Duration) *Supervisor {
	cp := s.clone()
	cp.failureDecay = decay
	return cp
}

// WithFailureThreshold sets the score above which the storm guard pauses (default
// 5.0). Only meaningful with [Supervisor.WithStormPause].
func (s *Supervisor) WithFailureThreshold(threshold float64) *Supervisor {
	cp := s.clone()
	cp.failureThreshold = threshold
	return cp
}

// StopWhen sets a predicate evaluated on each completed run (clean or not), before
// the restart policy. The first run it matches ends supervision with
// [StoppedByPredicate]. It never sees a run that failed to start.
func (s *Supervisor) StopWhen(predicate func(*Result) bool) *Supervisor {
	cp := s.clone()
	cp.stopWhen = predicate
	return cp
}

// WithRunner sets the [ProcessRunner] each incarnation runs through — the
// dependency-injection and test seam. The default is a [JobRunner] (a fresh
// kill-on-drop job per run).
func (s *Supervisor) WithRunner(r ProcessRunner) *Supervisor {
	cp := s.clone()
	cp.runner = r
	return cp
}

// Run supervises the command until a stop condition is met, returning the
// [SupervisionOutcome]. A nil error means supervision *concluded* (not that the
// command succeeded — check Final). It returns an error only when the caller's
// context is cancelled or a run that could not even start is the terminating one
// (a spawn failure with no further restart allowed).
func (s *Supervisor) Run(ctx context.Context) (*SupervisionOutcome, error) {
	runner := s.runner
	if runner == nil {
		runner = JobRunner{}
	}
	rng := s.rng
	if rng == nil {
		src := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // jitter, not security
		rng = src.Float64
	}
	inv := s.cmd.invocation()

	var (
		restarts    int
		stormPauses int
		storm       stormState
	)
	for {
		if ctx.Err() != nil {
			return nil, &CancelError{Program: s.cmd.program, Cause: ctx.Err()}
		}

		result, err := runner.Output(ctx, inv)

		var crashed bool
		if err != nil {
			if errors.Is(err, ErrCancelled) {
				return nil, err // a cancelled incarnation is terminal
			}
			crashed = true // a spawn/IO failure is retried like a crash
		} else {
			if s.stopWhen != nil && s.stopWhen(result) {
				return &SupervisionOutcome{Final: result, Restarts: restarts, Stopped: StoppedByPredicate, StormPauses: stormPauses}, nil
			}
			crashed = !result.Success()
		}

		if !s.policy.wantsRestart(crashed) {
			if err != nil {
				return nil, err
			}
			return &SupervisionOutcome{Final: result, Restarts: restarts, Stopped: StoppedPolicySatisfied, StormPauses: stormPauses}, nil
		}
		if s.maxRestarts >= 0 && restarts >= s.maxRestarts {
			if err != nil {
				return nil, err
			}
			return &SupervisionOutcome{Final: result, Restarts: restarts, Stopped: StoppedRestartsExhausted, StormPauses: stormPauses}, nil
		}

		// Storm guard (failure path only), then per-restart backoff.
		if crashed && s.stormPause > 0 {
			if storm.record(time.Now(), s.failureDecay) > s.failureThreshold {
				if !sleepCtx(ctx, applyJitter(s.stormPause, rng, s.jitter)) {
					return nil, &CancelError{Program: s.cmd.program, Cause: ctx.Err()}
				}
				stormPauses++
				storm.reset()
			}
		}
		delay := applyJitter(backoffDelay(restarts, s.backoffBase, s.factor, s.maxBackoff), rng, s.jitter)
		if !sleepCtx(ctx, delay) {
			return nil, &CancelError{Program: s.cmd.program, Cause: ctx.Err()}
		}
		restarts++
	}
}

// backoffDelay is the capped-exponential delay before restart n (0-based):
// min(base × factor^n, cap). A non-positive base is no delay; a factor below 1 or
// non-finite is treated as 1 (constant delay); overflow saturates at cap.
func backoffDelay(n int, base time.Duration, factor float64, limit time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	if limit <= 0 {
		limit = base
	}
	if !(factor >= 1) || math.IsInf(factor, 0) { // NaN, <1, or +Inf → constant
		factor = 1
	}
	scaled := float64(base) * math.Pow(factor, float64(n))
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled >= float64(limit) {
		return limit
	}
	return time.Duration(scaled)
}

// applyJitter spreads d by a uniform ±50% (× [0.5, 1.5)) when enabled, so a fleet
// of supervisors don't restart in lockstep. A non-positive d (and disabled jitter)
// is returned unchanged; an overflowing result saturates rather than wrapping.
func applyJitter(d time.Duration, rng func() float64, enabled bool) time.Duration {
	if !enabled || d <= 0 {
		return d
	}
	scaled := float64(d) * (0.5 + rng())
	if scaled <= 0 || math.IsNaN(scaled) {
		return d
	}
	if scaled >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(scaled)
}

// stormState is the decaying failure score backing the storm guard.
type stormState struct {
	score   float64
	last    time.Time
	hasLast bool
}

// record folds in the time-decay since the previous failure, then adds 1 for this
// failure, and returns the new score. A zero decay keeps no history (score stays
// 1); a poisoned (non-finite) score resets to 1.
func (st *stormState) record(now time.Time, decay time.Duration) float64 {
	score := st.score
	switch {
	case !st.hasLast, decay <= 0, math.IsNaN(score), math.IsInf(score, 0):
		score = 0
	default:
		dt := now.Sub(st.last).Seconds()
		score *= math.Pow(0.5, dt/decay.Seconds())
		if math.IsNaN(score) || math.IsInf(score, 0) {
			score = 0
		}
	}
	score++
	st.score = score
	st.last = now
	st.hasLast = true
	return score
}

// reset clears the storm score (after a pause), so a fresh storm must rebuild.
func (st *stormState) reset() {
	st.score = 0
	st.hasLast = false
}

// sleepCtx sleeps for d, returning true if it elapsed or false if ctx ended first.
// A non-positive d does not sleep; it just reports whether ctx is still live.
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

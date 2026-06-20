package processkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZelAnton/processkit-go/internal/sys"
)

// Invocation is the immutable description of a single run, handed to a
// [ProcessRunner]. It is what a test double matches on.
//
// Build it with keyed literals; new fields may be added in later versions. The
// slices (Args, Env, OkCodes) are borrowed — a runner must not retain or mutate
// them.
type Invocation struct {
	Program string
	Args    []string
	Dir     string   // working directory; "" inherits the parent's
	Env     []string // full environment as "KEY=VALUE"; nil inherits the parent's
	OkCodes []int    // exit codes treated as success in addition to 0
	Timeout time.Duration
	Stdin   io.Reader // standard input for the run, or nil for none (capture verbs only)
}

// ProcessRunner runs a command to completion and returns the captured [Result].
// It is processkit's dependency-injection and test seam: a fake runner needs no
// real subprocess. A non-zero exit is reported in the Result, not as an error.
type ProcessRunner interface {
	Output(ctx context.Context, inv Invocation) (*Result, error)
}

// waitDelay bounds cmd.Wait after the process exits or its context fires, in case
// a descendant still holds a captured pipe open; after it the pipes are
// force-closed so Wait can't hang on an escaped descendant.
const waitDelay = 5 * time.Second

// JobRunner is the real runner. Each command runs inside its own private,
// kill-on-drop job (a Windows Job Object, or a POSIX process group) so the whole
// tree — grandchildren included — dies with the run. The zero value is ready to use.
type JobRunner struct{ log runLog } // log is wired by Cmd.WithLogger; zero value is silent

// Output runs inv to completion inside a fresh job and captures stdout/stderr.
//
// Timeout semantics: the run's own deadline ([Cmd.WithTimeout] → inv.Timeout) is
// *captured* — the Result reports [Outcome.TimedOut] and no error. The caller's
// context ending the run (cancelled, or its own deadline elapsed) is instead an
// error ([*CancelError]); it wins over the run's own timeout and over a natural
// exit. A process that exits exactly as its own deadline fires is reported as a
// timeout (the ambiguity is resolved in favour of the deadline).
func (r JobRunner) Output(ctx context.Context, inv Invocation) (*Result, error) {
	parent := ctx
	runCtx := ctx
	if inv.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(parent, inv.Timeout)
		defer cancel()
	}
	if err := cancelledBy(parent, inv.Program); err != nil { // cancelled before we spawn anything
		return nil, err
	}

	cmd := exec.CommandContext(runCtx, inv.Program, inv.Args...)
	cmd.Dir = inv.Dir
	cmd.Env = inv.Env
	cmd.Stdin = inv.Stdin // nil → no stdin, as before
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	job, err := sys.NewJob(sys.Limits{}) // per-run containment carries no resource caps
	if err != nil {
		return nil, &StartError{Program: inv.Program, Err: err}
	}
	defer job.Close()
	if err := job.Configure(cmd); err != nil {
		// A per-run job carries no limits, so the Linux cgroup backend already fell
		// back to a process group at NewJob if the cgroup couldn't be made — Configure
		// here only sets SysProcAttr fields and is a containment step, never a limit
		// one (those are a Group facility, applied at NewGroup).
		return nil, &StartError{Program: inv.Program, Err: err}
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		// A start that failed because the caller cancelled is a cancellation, not a
		// spawn failure (mirrors the Assign-failure path below and Pipeline.Output).
		if cerr := cancelledBy(parent, inv.Program); cerr != nil {
			return nil, cerr
		}
		return nil, startErr(inv.Program, err)
	}
	if err := job.Assign(cmd); err != nil {
		// Containment failed — tear down whatever exists; never leak an orphan.
		_ = job.Kill()
		_ = cmd.Wait()
		if cerr := cancelledBy(parent, inv.Program); cerr != nil {
			return nil, cerr
		}
		return nil, &StartError{Program: inv.Program, Err: err}
	}

	r.log.spawned(inv.Program, cmd.Process.Pid, toMechanism(job.Mechanism()))
	waitErr := cmd.Wait()
	duration := time.Since(start)
	_ = job.Kill() // reap any grandchildren that outlived the direct child

	// The caller's context ending the run wins over everything: no captured Result.
	if cerr := cancelledBy(parent, inv.Program); cerr != nil {
		r.log.cancelled(inv.Program)
		return nil, cerr
	}

	deadlineFired := inv.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded)
	switch {
	case deadlineFired:
		r.log.timedOut(inv.Program, inv.Timeout)
	case cmd.ProcessState == nil && waitErr != nil:
		// No status and a wait error with no deadline/cancel — a genuine spawn/wait failure.
		return nil, &StartError{Program: inv.Program, Err: waitErr}
	}
	outcome := resolveOutcome(cmd.ProcessState, deadlineFired)
	r.log.exited(inv.Program, outcome, duration)

	return &Result{
		program:   inv.Program,
		args:      append([]string(nil), inv.Args...),
		outcome:   outcome,
		stdout:    stdout.Bytes(),
		stderr:    normalizeNewlines(stderr.Bytes()),
		duration:  duration,
		okCodes:   append([]int(nil), inv.OkCodes...),
		mechanism: toMechanism(job.Mechanism()),
	}, nil
}

func toMechanism(m sys.Mechanism) Mechanism {
	switch m {
	case sys.JobObject:
		return MechanismJobObject
	case sys.ProcessGroup:
		return MechanismProcessGroup
	case sys.CgroupV2:
		return MechanismCgroupV2
	default:
		return MechanismUnknown
	}
}

// resolveOutcome maps a finished run to an [Outcome] with the fixed precedence the
// spawn paths share: the run's own deadline wins over the exit status (a process
// that exits exactly as its deadline fires is reported as a timeout), then the
// captured ProcessState, then a clean exit(0). ps may be nil (no state captured).
func resolveOutcome(ps *os.ProcessState, deadlineFired bool) Outcome {
	switch {
	case deadlineFired:
		return timedOut()
	case ps != nil:
		return outcomeOf(ps)
	default:
		return exited(0)
	}
}

// cancelledBy returns a [*CancelError] when the caller's context ended the run —
// which wins over the run's own outcome — or nil otherwise. The single home for the
// "caller cancel is an error, with no captured output" rule.
func cancelledBy(parent context.Context, program string) error {
	if parent.Err() != nil {
		return &CancelError{Program: program, Cause: parent.Err()}
	}
	return nil
}

// startErr classifies a failed cmd.Start: a program that could not be located is a
// [*NotFoundError] (matching [ErrNotFound]); anything else is a [*StartError].
func startErr(program string, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return &NotFoundError{Program: program, Searched: searchedPath(program)}
	}
	return &StartError{Program: program, Err: err}
}

// searchedPath returns the PATH directories an exec lookup would have searched
// for program — for the [NotFoundError] diagnostic. A program that already
// contains a path separator is not looked up on PATH, so it returns nil.
func searchedPath(program string) []string {
	if strings.ContainsAny(program, `/\`) {
		return nil
	}
	path := os.Getenv("PATH")
	if path == "" {
		return nil
	}
	return filepath.SplitList(path)
}

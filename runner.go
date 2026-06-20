package processkit

import (
	"bytes"
	"context"
	"errors"
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
type JobRunner struct{}

// Output runs inv to completion inside a fresh job and captures stdout/stderr.
//
// Timeout semantics: the run's own deadline ([Cmd.WithTimeout] → inv.Timeout) is
// *captured* — the Result reports [Outcome.TimedOut] and no error. The caller's
// context ending the run (cancelled, or its own deadline elapsed) is instead an
// error ([*CancelError]); it wins over the run's own timeout and over a natural
// exit. A process that exits exactly as its own deadline fires is reported as a
// timeout (the ambiguity is resolved in favour of the deadline).
func (JobRunner) Output(ctx context.Context, inv Invocation) (*Result, error) {
	parent := ctx
	runCtx := ctx
	if inv.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(parent, inv.Timeout)
		defer cancel()
	}
	if parent.Err() != nil { // already cancelled / expired before we spawn anything
		return nil, &CancelError{Program: inv.Program, Cause: parent.Err()}
	}

	cmd := exec.CommandContext(runCtx, inv.Program, inv.Args...)
	cmd.Dir = inv.Dir
	cmd.Env = inv.Env
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
		// TODO(cgroup): a future cgroup Configure failure (couldn't create/enable
		// the cgroup) should surface as ErrResourceLimit, not StartError. Latent
		// today — the Job Object and pgroup backends never fail here.
		return nil, &StartError{Program: inv.Program, Err: err}
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		// A start that failed because the caller cancelled is a cancellation, not a
		// spawn failure (mirrors the Assign-failure path below and Pipeline.Output).
		if parent.Err() != nil {
			return nil, &CancelError{Program: inv.Program, Cause: parent.Err()}
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &NotFoundError{Program: inv.Program, Searched: searchedPath(inv.Program)}
		}
		return nil, &StartError{Program: inv.Program, Err: err}
	}
	if err := job.Assign(cmd); err != nil {
		// Containment failed — tear down whatever exists; never leak an orphan.
		_ = job.Kill()
		_ = cmd.Wait()
		if parent.Err() != nil {
			return nil, &CancelError{Program: inv.Program, Cause: parent.Err()}
		}
		return nil, &StartError{Program: inv.Program, Err: err}
	}

	waitErr := cmd.Wait()
	duration := time.Since(start)
	_ = job.Kill() // reap any grandchildren that outlived the direct child

	// The caller's context ending the run wins over everything: no captured Result.
	if parent.Err() != nil {
		return nil, &CancelError{Program: inv.Program, Cause: parent.Err()}
	}

	var outcome Outcome
	switch {
	case inv.Timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		outcome = timedOut()
	case cmd.ProcessState != nil:
		outcome = outcomeOf(cmd.ProcessState)
	case waitErr != nil:
		return nil, &StartError{Program: inv.Program, Err: waitErr}
	default:
		outcome = exited(0)
	}

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
	default:
		return MechanismUnknown
	}
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

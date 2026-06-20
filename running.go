package processkit

import (
	"context"
	"os/exec"
)

// RunningProcess is a live handle to a process started in a [Group]. Wait for it
// to exit, read its pid, or kill it. (Output streaming and interactive stdin land
// on this type in a later stage; a Group-started process discards its output for
// now.)
type RunningProcess struct {
	cmd       *exec.Cmd
	program   string
	mechanism Mechanism

	done    chan struct{} // closed when the process has been reaped
	outcome Outcome
	waitErr error
}

// reap waits for the process and records its outcome, then closes done. It runs
// in its own goroutine for the lifetime of the process.
func (p *RunningProcess) reap() {
	err := p.cmd.Wait()
	switch {
	case p.cmd.ProcessState != nil:
		p.outcome = outcomeOf(p.cmd.ProcessState)
	case err != nil:
		p.waitErr = err
	default:
		p.outcome = exited(0)
	}
	close(p.done)
}

// Pid returns the process id, or 0 if it never started.
func (p *RunningProcess) Pid() int {
	if p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// Wait blocks until the process exits and returns its [Outcome], or returns early
// with ctx's bare error (context.Canceled / context.DeadlineExceeded, not an
// [ErrCancelled]) if the context is done first — in which case the process keeps
// running; kill it via the owning [Group] or [RunningProcess.Kill].
func (p *RunningProcess) Wait(ctx context.Context) (Outcome, error) {
	select {
	case <-p.done:
		return p.outcome, p.waitErr
	case <-ctx.Done():
		return Outcome{}, ctx.Err()
	}
}

// Kill terminates this process. Its descendants are reaped when the owning
// [Group] is closed; killing one process does not tear down the whole group.
func (p *RunningProcess) Kill() error {
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

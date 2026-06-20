package processkit

import (
	"context"
	"os/exec"
	"sync"
	"sync/atomic"
)

// RunningProcess is a live handle to a process started in a [Group]. Wait for it
// to exit, read its pid, kill it, or — when the [Group.Start] was given streaming
// options — read its output line by line via [RunningProcess.Lines] (or the
// [OnStdoutLine] / [OnStderrLine] callbacks and [WithStdout] / [WithStderr] tees).
type RunningProcess struct {
	cmd       *exec.Cmd
	program   string
	mechanism Mechanism

	done    chan struct{} // closed when the process has been reaped
	outcome Outcome
	waitErr error

	// Streaming state, populated by Group.Start when stream options are given.
	lines    chan Line      // merged stdout+stderr lines; nil unless StreamLines()
	dropped  int64          // lines dropped under OverflowDropNewest (accessed atomically)
	drainWG  sync.WaitGroup // gates reap() until the output pipes hit EOF
	stop     chan struct{}  // closed on Close/Kill to release a backpressured drain
	stopOnce sync.Once
}

// signalStop releases any drain goroutine blocked sending to a full Lines()
// channel, so teardown (Group.Close or Kill) can never leave it hanging. Safe to
// call repeatedly and on a non-streaming process.
func (p *RunningProcess) signalStop() {
	if p.stop != nil {
		p.stopOnce.Do(func() { close(p.stop) })
	}
}

// reap drains the output pipes, waits for the process, records its outcome, and
// closes the line channel then done. It runs in its own goroutine for the
// lifetime of the process. The drain must finish before Wait: when stdout/stderr
// are piped, Wait closes the pipes, so reading has to complete first.
func (p *RunningProcess) reap() {
	p.drainWG.Wait()
	err := p.cmd.Wait()
	switch {
	case p.cmd.ProcessState != nil:
		// The exit status is authoritative: a late pipe-close (exec.ErrWaitDelay)
		// or similar non-fatal Wait error is intentionally ignored when we have a
		// ProcessState. These fields are read by Wait/WaitAny/WaitAll/Members only
		// after p.done is closed below, so the writes are safely published.
		p.outcome = outcomeOf(p.cmd.ProcessState)
	case err != nil:
		p.waitErr = err
	default:
		p.outcome = exited(0)
	}
	if p.lines != nil {
		close(p.lines)
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

// Lines returns the merged stdout/stderr line channel for a process started with
// [StreamLines]; each [Line] is tagged with its [StreamID]. The channel closes
// once both streams reach EOF (the process has produced all its output). If the
// start did not enable streaming, Lines returns an already-closed channel, so
// ranging over it is always safe. Drain it until it closes, or cancel the start
// context, so a slow reader can't stall the process under [OverflowBlock].
func (p *RunningProcess) Lines() <-chan Line {
	if p.lines == nil {
		return closedLineChan
	}
	return p.lines
}

// DroppedLines reports how many lines the [OverflowDropNewest] policy discarded
// because the [RunningProcess.Lines] channel was full. It counts policy drops
// only and is always 0 under the default [OverflowBlock] (a line lost when
// cancellation or teardown releases a backpressured send is not counted). Read it
// after the channel has closed for the final count.
func (p *RunningProcess) DroppedLines() int {
	return int(atomic.LoadInt64(&p.dropped))
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
	p.signalStop()
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

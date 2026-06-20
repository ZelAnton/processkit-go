package processkit

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/ZelAnton/processkit-go/internal/sys"
)

// sigTERM is the signal number for SIGTERM — the graceful-shutdown signal. It is
// only delivered on Unix (Windows has no signal tier; shutdown is an atomic kill).
const sigTERM = 15

// defaultShutdownGrace is how long [Group.Shutdown] waits for members to exit
// before hard-killing survivors, when no grace is given.
const defaultShutdownGrace = 5 * time.Second

// errGroupClosed is returned (wrapped in a [*StartError]) when Start races a
// concurrent Close and loses — the child is torn down rather than left orphaned.
var errGroupClosed = errors.New("processkit: Start on a closed group")

// ShutdownOption configures [Group.Shutdown].
type ShutdownOption func(*shutdownConfig)

type shutdownConfig struct{ grace time.Duration }

// ShutdownGrace sets how long graceful shutdown waits for members to exit before
// hard-killing the survivors (Unix only — Windows shutdown is an atomic kill).
func ShutdownGrace(d time.Duration) ShutdownOption {
	return func(c *shutdownConfig) { c.grace = d }
}

// StartOption configures a single [Group.Start] — its output streaming, line
// callbacks, and interactive stdin. See [WithStdout], [OnStdoutLine],
// [StreamLines], [WithStdin], and the other With/On options. The zero set
// discards the process's output.
type StartOption func(*startConfig)

// Group is an explicit, shared kill-on-drop container for a set of processes.
// Every process started into the group — and everything those processes spawn —
// lives in one OS container (a Windows Job Object, or POSIX process groups), so
// [Group.Close] reaps the whole tree, grandchildren included. Always pair a Group
// with `defer group.Close()`.
type Group struct {
	job       sys.Job
	mechanism Mechanism
	clk       clock

	mu     sync.Mutex
	procs  []*RunningProcess
	closed bool
}

// NewGroup creates an empty process group.
func NewGroup() (*Group, error) {
	job, err := sys.NewJob()
	if err != nil {
		return nil, &StartError{Program: "<group>", Err: err}
	}
	return &Group{job: job, mechanism: toMechanism(job.Mechanism()), clk: realClock{}}, nil
}

// Mechanism reports the containment mechanism the group is using.
func (g *Group) Mechanism() Mechanism { return g.mechanism }

// Start runs cmd as a member of the group and returns a live handle. The process
// keeps running until it exits, is killed, or the group is closed.
//
// Start uses cmd's program, arguments, working directory, and environment, but
// NOT its WithTimeout / WithOkCodes / WithRunner — those configure the capture
// verbs ([Cmd.Output] etc.), not a live start. Bound a started process with the
// ctx you pass and tear it down with [RunningProcess.Kill] or [Group.Close].
//
// By default a group-started process discards its stdout/stderr. Pass stream
// [StartOption]s to observe its I/O: [StreamLines] (then range [RunningProcess.Lines]),
// the [OnStdoutLine] / [OnStderrLine] callbacks, the [WithStdout] / [WithStderr]
// tees, and [WithStdin] for interactive input.
func (g *Group) Start(ctx context.Context, cmd *Cmd, opts ...StartOption) (*RunningProcess, error) {
	var scfg startConfig
	for _, o := range opts {
		o(&scfg)
	}
	scfg.resolve()

	inv := cmd.invocation()
	ecmd := exec.CommandContext(ctx, inv.Program, inv.Args...)
	ecmd.Dir = inv.Dir
	ecmd.Env = inv.Env
	ecmd.WaitDelay = waitDelay

	// Configure before opening any pipe, so a Configure failure (e.g. a future
	// cgroup backend) can't strand a half-created pipe's file descriptors.
	if err := g.job.Configure(ecmd); err != nil {
		return nil, &StartError{Program: inv.Program, Err: err}
	}
	stdoutR, stderrR, err := scfg.preparePipes(ecmd)
	if err != nil {
		return nil, &StartError{Program: inv.Program, Err: err}
	}
	if err := ecmd.Start(); err != nil {
		closePipes(stdoutR, stderrR)
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &NotFoundError{Program: inv.Program, Searched: searchedPath(inv.Program)}
		}
		return nil, &StartError{Program: inv.Program, Err: err}
	}

	// Contain and register the child under g.mu, serialized against Close: either
	// we Assign and record it before Close snapshots the members (so Close tears it
	// down), or we observe g.closed and tear it down ourselves. Never an orphan,
	// and never a drain/reap goroutine that outlives Close. Holding the lock across
	// Assign is also what keeps the Unix pgid list from being killed between Assign
	// and registration.
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		_ = ecmd.Process.Kill()
		_ = ecmd.Wait()
		closePipes(stdoutR, stderrR)
		return nil, &StartError{Program: inv.Program, Err: errGroupClosed}
	}
	if err := g.job.Assign(ecmd); err != nil {
		g.mu.Unlock()
		// Containment failed: kill the direct child. On a shared group we can't
		// job.Kill (it would reap siblings), so a grandchild this child already
		// spawned is only reaped at Group.Close — an accepted edge case.
		_ = ecmd.Process.Kill()
		_ = ecmd.Wait()
		closePipes(stdoutR, stderrR)
		return nil, &StartError{Program: inv.Program, Err: err}
	}
	p := &RunningProcess{
		cmd:       ecmd,
		program:   inv.Program,
		mechanism: g.mechanism,
		done:      make(chan struct{}),
		stop:      make(chan struct{}),
	}
	g.procs = append(g.procs, p)
	g.mu.Unlock()

	// Launch the drains and reaper after registration so a concurrent Close (which
	// has now snapshotted p) reliably reaches them via job.Kill + signalStop.
	scfg.launchDrains(p, ctx, stdoutR, stderrR)
	go p.reap()
	return p, nil
}

// Members returns a snapshot of the live processes' pids.
func (g *Group) Members() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	pids := make([]int, 0, len(g.procs))
	for _, p := range g.procs {
		select {
		case <-p.done: // exited — not a live member
		default:
			if pid := p.Pid(); pid != 0 {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// Close hard-kills every process in the group (grandchildren included) and
// releases the container. Idempotent.
func (g *Group) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	procs := append([]*RunningProcess(nil), g.procs...)
	g.mu.Unlock()

	killErr := g.job.Kill()
	closeErr := g.job.Close()
	// Release any streaming drain blocked on a full, abandoned Lines() channel, so
	// Close never leaks the drain/reap goroutines.
	for _, p := range procs {
		p.signalStop()
	}
	if killErr != nil {
		return killErr
	}
	return closeErr
}

// Shutdown tears the group down gracefully: on Unix it sends SIGTERM to the whole
// tree, waits up to the grace period (default 5s; set with [ShutdownGrace]) for
// members to exit, then hard-kills the survivors and closes the container. On
// Windows there is no signal tier, so it is an immediate atomic kill (the grace
// is ignored). Idempotent via [Group.Close].
func (g *Group) Shutdown(ctx context.Context, opts ...ShutdownOption) error {
	cfg := shutdownConfig{grace: defaultShutdownGrace}
	for _, o := range opts {
		o(&cfg)
	}
	if err := g.job.Signal(sigTERM); err != nil {
		// Unsupported (Windows) or a delivery failure — fall back to a hard kill.
		return g.Close()
	}
	g.waitGrace(ctx, cfg.grace)
	return g.Close()
}

// waitGrace blocks until every member has exited, grace elapses, or ctx is done.
func (g *Group) waitGrace(ctx context.Context, grace time.Duration) {
	g.mu.Lock()
	procs := append([]*RunningProcess(nil), g.procs...)
	g.mu.Unlock()

	deadline, stop := g.clk.NewTimer(grace)
	defer stop()
	for _, p := range procs {
		select {
		case <-p.done:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

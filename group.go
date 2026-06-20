package processkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/ZelAnton/processkit-go/internal/sys"
)

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

// GroupOption configures a [Group] at creation — currently the whole-tree resource
// caps [WithMemoryMax], [WithMaxProcesses], and [WithCPUQuota]. Limits are applied
// to the OS container when the group is created; they cannot be changed afterwards.
type GroupOption func(*groupConfig)

type groupConfig struct {
	limits sys.Limits
	log    runLog
}

// WithLogger configures a [Group] to emit structured [log/slog] events — child
// spawn and exit, group teardown, graceful shutdown, and adoption. The default is
// no logging; pass nil to disable. Events carry the program name, pid, mechanism,
// outcome, and durations, but NEVER arguments, environment, working directory, or
// output. (As a [GroupOption] it sits alongside [WithMemoryMax] etc.)
func WithLogger(logger *slog.Logger) GroupOption {
	return func(c *groupConfig) { c.log = runLog{logger} }
}

// WithMemoryMax caps the whole tree's memory at bytes (which must be > 0). Enforced
// by a Windows Job Object; on a mechanism without a whole-tree limit primitive
// [NewGroup] returns a [*ResourceLimitError] rather than an unbounded group.
func WithMemoryMax(bytes uint64) GroupOption {
	return func(c *groupConfig) { c.limits.MemoryMax, c.limits.HasMemoryMax = bytes, true }
}

// WithMaxProcesses caps the number of live processes in the tree at n (> 0). On
// Windows the Job Object's active-process limit refuses the process that would
// exceed it: a [Group.Start] (or [Group.Adopt]) past the cap fails — the process is
// rejected, never silently admitted — so the failure surfaces through that call,
// not as a creation-time error. See [WithMemoryMax] for the unsupported-mechanism
// behaviour.
func WithMaxProcesses(n uint32) GroupOption {
	return func(c *groupConfig) { c.limits.MaxProcesses, c.limits.HasMaxProcesses = n, true }
}

// WithCPUQuota caps the tree's CPU at cores cores' worth (0.5 = half a core, 2.0 =
// two cores; must be finite and > 0). On Windows the hard cap is expressed against
// total system CPU and so is approximate; a quota at or above the core count
// saturates at 100%. See [WithMemoryMax] for the unsupported-mechanism behaviour.
func WithCPUQuota(cores float64) GroupOption {
	return func(c *groupConfig) { c.limits.CPUQuota, c.limits.HasCPUQuota = cores, true }
}

// Group is an explicit, shared kill-on-drop container for a set of processes.
// Every process started into the group — and everything those processes spawn —
// lives in one OS container (a Windows Job Object, or POSIX process groups), so
// [Group.Close] reaps the whole tree, grandchildren included. Always pair a Group
// with `defer group.Close()`.
type Group struct {
	job       sys.Job
	mechanism Mechanism
	clk       clock
	log       runLog

	mu     sync.Mutex
	procs  []*RunningProcess
	closed bool
}

// NewGroup creates an empty process group. Pass [WithMemoryMax],
// [WithMaxProcesses], or [WithCPUQuota] to cap the whole tree's resources; those
// caps are applied to the OS container now. If a cap is invalid, or the active
// mechanism can't enforce it (no whole-tree limit primitive — every Unix backend
// today), NewGroup returns a [*ResourceLimitError] (matching [ErrResourceLimit])
// rather than handing back a silently-unbounded group.
func NewGroup(opts ...GroupOption) (*Group, error) {
	var cfg groupConfig
	for _, o := range opts {
		o(&cfg)
	}
	if err := validateLimits(cfg.limits); err != nil {
		return nil, err
	}
	job, err := sys.NewJob(cfg.limits)
	if err != nil {
		// A creation failure with caps requested is, by construction, a limit that
		// couldn't be enforced (the backend rejected it or has no primitive).
		if cfg.limits.Any() {
			return nil, &ResourceLimitError{Reason: err.Error(), Cause: err}
		}
		return nil, &StartError{Program: "<group>", Err: err}
	}
	return &Group{job: job, mechanism: toMechanism(job.Mechanism()), clk: realClock{}, log: cfg.log}, nil
}

// validateLimits rejects nonsensical caps before touching the OS, so a typo
// surfaces as a clear [*ResourceLimitError] rather than an opaque kernel error.
func validateLimits(l sys.Limits) error {
	if l.HasMemoryMax && l.MemoryMax == 0 {
		return &ResourceLimitError{Limit: "memory", Reason: "memory max must be greater than 0"}
	}
	if l.HasMaxProcesses && l.MaxProcesses == 0 {
		return &ResourceLimitError{Limit: "processes", Reason: "max processes must be greater than 0"}
	}
	if l.HasCPUQuota && !(l.CPUQuota > 0 && !math.IsInf(l.CPUQuota, 0)) {
		return &ResourceLimitError{Limit: "cpu", Reason: "cpu quota must be a finite value greater than 0"}
	}
	return nil
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
	// cgroup backend) can't strand a half-created pipe's file descriptors. Resource
	// caps are applied once at NewGroup, not per child, so a Configure failure here
	// is a containment failure (a StartError), never an unenforceable-limit one.
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
		startTime: time.Now(),
		done:      make(chan struct{}),
		stop:      make(chan struct{}),
		log:       g.log,
	}
	g.procs = append(g.procs, p)
	g.mu.Unlock()
	g.log.spawned(inv.Program, ecmd.Process.Pid, g.mechanism)

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

// Signal sends sig to every process in the group (and everything they spawned).
// [SignalKill] works on every platform — it is the atomic whole-tree kill, like
// [Group.Close]; the other signals are delivered on Unix but return
// [ErrUnsupported] on Windows, whose Job Object has no signal tier. Signalling a
// group whose members have already exited is a no-op success.
func (g *Group) Signal(sig Signal) error {
	if sig.isKill() {
		return g.job.Kill()
	}
	return mapUnsupported(g.job.Signal(sig.number()), "signal "+sig.String())
}

// Suspend freezes every process in the group; [Group.Resume] thaws them. On Unix
// this is SIGSTOP / SIGCONT to the whole tree. On Windows it returns
// [ErrUnsupported] (a Job Object has no freeze). A suspended group is still killed
// by [Group.Close]; resume before [Group.Shutdown], as a frozen tree can't act on
// SIGTERM.
func (g *Group) Suspend() error { return mapUnsupported(g.job.Suspend(), "suspend") }

// Resume thaws a group frozen by [Group.Suspend].
func (g *Group) Resume() error { return mapUnsupported(g.job.Resume(), "resume") }

// Adopt pulls an externally-started process — one you started yourself (e.g. via
// os/exec) and have NOT yet waited on — into the group, so it is torn down when the
// group is closed. Pass its [os.Process] (e.g. exec.Cmd.Process).
//
// Containment is best-effort: on Windows the process joins the Job Object; in a
// POSIX process group it becomes a group leader when it can (capturing its future
// descendants too), or is tracked individually if it has already exec'd. A process
// that has already exited is a benign success. The adopted process is not listed by
// [Group.Members] (that reports the processes you Started through the group).
//
// An adopted process counts against [WithMaxProcesses]: on a group capped at its
// active-process limit, Adopt is refused (the process is not pulled in) and returns
// the assignment error.
func (g *Group) Adopt(p *os.Process) error {
	if p == nil {
		return errors.New("processkit: Adopt(nil)")
	}
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return errGroupClosed
	}
	err := mapUnsupported(g.job.Adopt(p.Pid), "adopt")
	if err == nil {
		g.log.adopted(p.Pid, g.mechanism)
	}
	return err
}

// mapUnsupported converts the internal sys.ErrUnsupported into the public
// [ErrUnsupported], tagged with the operation, and passes any other error through.
func mapUnsupported(err error, op string) error {
	if errors.Is(err, sys.ErrUnsupported) {
		return fmt.Errorf("processkit: %s is not supported on this platform: %w", op, ErrUnsupported)
	}
	return err
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
	g.log.terminating(g.mechanism)

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
	g.log.shuttingDown(g.mechanism, cfg.grace)
	if err := g.job.Signal(SignalTerm.number()); err != nil {
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

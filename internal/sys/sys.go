//go:build windows || unix

// Package sys is the platform layer behind processkit's whole-tree, no-orphan
// containment. A Job owns one or more started processes and everything they
// spawn, so the whole group can be killed as a unit. One implementation is
// compiled per target, all satisfying the same Job interface.
package sys

import (
	"errors"
	"math"
	"os/exec"
	"time"
)

// Stats is a whole-job resource snapshot. CPU time and peak memory are optional —
// the Has flags say whether the backend could read them (the process-group backend
// can't, so it reports only the count).
type Stats struct {
	ActiveProcesses int
	CPUTime         time.Duration
	HasCPU          bool
	PeakMemoryBytes uint64
	HasMem          bool
}

// ProcMetrics is a single process's resource usage. Each metric is independently
// optional (Has flag), so a partial read still returns what it got.
type ProcMetrics struct {
	CPUTime    time.Duration
	HasCPU     bool
	PeakMemory uint64
	HasMem     bool
}

// Mechanism reports which OS primitive a Job uses (mapped to the public
// processkit.Mechanism by the caller, which can't import this internal package).
type Mechanism int

const (
	Unknown Mechanism = iota
	JobObject
	CgroupV2
	ProcessGroup
)

// ErrUnsupported is returned by an operation a platform can't perform (e.g. a
// non-kill Signal on Windows, whose Job Object only supports terminate).
var ErrUnsupported = errors.New("sys: operation not supported on this platform")

// Job contains a set of started process trees. A Job may hold one child (a
// private per-run job) or many (a shared group). Per child:
//
//	j.Configure(cmd)   // set SysProcAttr for containment, before Start
//	cmd.Start()
//	j.Assign(cmd)      // contain the started child (kills just it on failure)
//
// Then the whole group is torn down with Kill (or, gracefully, Signal + Kill)
// and Close.
type Job interface {
	// Configure prepares cmd for containment before it is started. It may create
	// OS resources (e.g. a cgroup) and fail — the caller must not Start cmd if it
	// returns an error.
	Configure(cmd *exec.Cmd) error
	// Assign contains a just-started child. May be called many times (a shared
	// group). On any failure it leaves no uncontained survivor (terminating just
	// that child if needed) and returns the error.
	Assign(cmd *exec.Cmd) error
	// Signal broadcasts sig (a signal number) to every member. Returns
	// ErrUnsupported where a platform can't deliver it (Windows supports only the
	// terminate path — use Kill).
	Signal(sig int) error
	// Suspend freezes every member; Resume thaws them. Returns ErrUnsupported where
	// a platform can't (Windows, here). Unix uses SIGSTOP / SIGCONT.
	Suspend() error
	Resume() error
	// Adopt pulls an externally-started process (by pid) into the job's
	// containment, so it is torn down with the group. Best-effort: an exited pid is
	// a success; the containment may be degraded (a process group can't always
	// reparent an already-exec'd child — it is then tracked individually).
	Adopt(pid int) error
	// Kill hard-kills every member. Idempotent; a group that already exited is
	// success.
	Kill() error
	// Close releases any OS handles held by the job. On Windows, if Kill was not
	// called first, closing the last job handle itself reaps the tree
	// (KILL_ON_JOB_CLOSE) — so call Kill before Close.
	Close() error
	// Mechanism reports the containment actually in effect.
	Mechanism() Mechanism
	// Stats reports a whole-tree resource snapshot. The Job Object backend reports
	// CPU + peak memory + an exact count; the process-group backend reports only the
	// count (CPU/memory unavailable without a cgroup).
	Stats() (Stats, error)
}

// NewJob creates a fresh, empty job for the current platform.
func NewJob() (Job, error) { return newJob() }

// ProcessMetrics reads one process's resource usage by pid. It never errors: a
// metric it can't read (process gone, permission, unsupported platform) is simply
// reported as unavailable (its Has flag false).
func ProcessMetrics(pid int) ProcMetrics { return processMetrics(pid) }

// saturatingMulU64 multiplies, clamping to MaxUint64 instead of overflowing.
func saturatingMulU64(a, b uint64) uint64 {
	if a != 0 && b > math.MaxUint64/a {
		return math.MaxUint64
	}
	return a * b
}

// nanosFromUnit converts count*unit nanoseconds, clamping to MaxInt64. count must
// be non-negative; unit is the per-count nanoseconds (e.g. 100 for a FILETIME
// 100-ns tick).
func nanosFromUnit(count, unit int64) time.Duration {
	if unit <= 0 {
		return 0 // invalid unit; no sensible conversion (defensive — never reached)
	}
	if count < 0 || count > math.MaxInt64/unit {
		return time.Duration(math.MaxInt64) // overflow / wrapped counter → saturate
	}
	return time.Duration(count * unit)
}

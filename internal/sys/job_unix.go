//go:build unix

package sys

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

// pgroupJob contains a set of trees via POSIX process groups: each child is made
// a group leader (Setpgid, so pgid == its pid) and teardown is killpg over every
// tracked group. This is the mechanism on every Unix (Linux, macOS, the BSDs)
// today. Weaker than a Job Object — a child that calls setsid escapes its group.
// Teardown and Signal/Suspend are pid-based, so they carry the usual small
// pid-reuse window: if a leader is reaped and its pid recycled before a Kill /
// Signal / Suspend runs, the operation could hit an unrelated process group (a
// stray SIGSTOP is worse than a stray kill — it wedges a victim). The window is
// only reachable for a long-lived group that outlives some of its members.
type pgroupJob struct {
	mu    sync.Mutex // guards pgids/solo: Assign/Adopt append while Signal/Kill read
	pgids []int      // process-group leaders (signalled by negative target)
	solo  []int      // individually-tracked adopted pids (no descendant capture)
}

func newJob(limits Limits) (Job, error) {
	// A POSIX process group has no resource accounting, so a requested cap can't be
	// honoured here — fail fast rather than hand back an unbounded tree the caller
	// believes is capped. (A Linux cgroup-v2 backend that enforces these is a
	// planned addition; until then every Unix takes this path.)
	if limits.Any() {
		return nil, errors.New("sys: resource limits require a cgroup or Job Object; unavailable on this target")
	}
	return &pgroupJob{}, nil
}

func (j *pgroupJob) Configure(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// New process group with pgid == child pid; descendants inherit it.
	cmd.SysProcAttr.Setpgid = true
	return nil
}

func (j *pgroupJob) Assign(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("sys: Assign called before the process started")
	}
	// With Setpgid and no Pgid set, the kernel makes the child its own group
	// leader, so the group id equals the child pid.
	j.mu.Lock()
	j.pgids = append(j.pgids, cmd.Process.Pid)
	j.mu.Unlock()
	return nil
}

func (j *pgroupJob) Signal(sig int) error {
	j.mu.Lock()
	pgids := append([]int(nil), j.pgids...)
	solo := append([]int(nil), j.solo...)
	j.mu.Unlock()

	var firstErr error
	for _, pgid := range pgids {
		deliver(-pgid, sig, &firstErr) // negative target: the whole process group
	}
	for _, pid := range solo {
		deliver(pid, sig, &firstErr) // a single adopted process
	}
	return firstErr
}

// deliver sends sig to target (a pid, or a negative process-group id), recording
// the first real error. Two errno values mean the target is effectively gone, both
// benign for an idempotent teardown of a group we own:
//
//	ESRCH — no such process / group (already reaped).
//	EPERM — on macOS/BSD, signalling a target whose only remaining members are
//	        unreaped zombies returns EPERM, not ESRCH; they are already dead.
//	        (Linux returns ESRCH or success here.)
func deliver(target, sig int, firstErr *error) {
	if err := syscall.Kill(target, syscall.Signal(sig)); err != nil &&
		!errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		if *firstErr == nil {
			*firstErr = err
		}
	}
}

// Suspend freezes the whole group with SIGSTOP; Resume thaws it with SIGCONT.
func (j *pgroupJob) Suspend() error { return j.Signal(int(syscall.SIGSTOP)) }
func (j *pgroupJob) Resume() error  { return j.Signal(int(syscall.SIGCONT)) }

// Adopt pulls an externally-started process into the group. It tries to make the
// process its own group leader (setpgid) so its future descendants are captured
// too. setpgid commonly fails — EACCES once the child has exec'd, or ESRCH/EPERM
// for a process we did not fork; whenever it fails, if the process is still alive
// and we can signal it, it is contained individually ("solo": signalled and killed
// with the group, but without descendant capture). An already-dead or
// unsignallable pid is a benign no-op.
func (j *pgroupJob) Adopt(pid int) error {
	if err := syscall.Setpgid(pid, pid); err == nil {
		j.mu.Lock()
		j.pgids = append(j.pgids, pid)
		j.mu.Unlock()
		return nil
	}
	if syscall.Kill(pid, 0) == nil { // alive and signallable → contain it solo
		j.trackSolo(pid)
	}
	return nil
}

func (j *pgroupJob) trackSolo(pid int) {
	j.mu.Lock()
	j.solo = append(j.solo, pid)
	j.mu.Unlock()
}

func (j *pgroupJob) Kill() error { return j.Signal(int(syscall.SIGKILL)) }

func (j *pgroupJob) Close() error { return nil }

func (j *pgroupJob) Mechanism() Mechanism { return ProcessGroup }

// Stats reports the live member count only — a process group has no kernel
// resource accumulator, so CPU and memory are unavailable without a cgroup. It
// counts process groups (a contained child that forks helpers counts once) plus
// solo-adopted pids that are still signallable.
func (j *pgroupJob) Stats() (Stats, error) {
	j.mu.Lock()
	pgids := append([]int(nil), j.pgids...)
	solo := append([]int(nil), j.solo...)
	j.mu.Unlock()

	n := 0
	for _, pgid := range pgids {
		if syscall.Kill(-pgid, 0) == nil { // a signal-0 probe: the group is still alive
			n++
		}
	}
	for _, pid := range solo {
		if syscall.Kill(pid, 0) == nil {
			n++
		}
	}
	return Stats{ActiveProcesses: n}, nil
}

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

func newJob() (Job, error) { return &pgroupJob{}, nil }

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
// too; if the process has already exec'd (EACCES) or can't be moved (EPERM), it is
// tracked individually ("solo") instead — contained (signalled/killed with the
// group), but without descendant capture. A pid we don't own (setpgid → ESRCH) is
// solo-tracked too if it is still alive and signallable; an already-dead pid is a
// benign no-op.
func (j *pgroupJob) Adopt(pid int) error {
	switch err := syscall.Setpgid(pid, pid); {
	case err == nil:
		j.mu.Lock()
		j.pgids = append(j.pgids, pid)
		j.mu.Unlock()
		return nil
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		j.trackSolo(pid)
		return nil
	case errors.Is(err, syscall.ESRCH):
		// Not our child. If it is alive and we can signal it, contain it solo;
		// otherwise (gone, or no permission to signal) there is nothing to do.
		if syscall.Kill(pid, 0) == nil {
			j.trackSolo(pid)
		}
		return nil
	default:
		return err
	}
}

func (j *pgroupJob) trackSolo(pid int) {
	j.mu.Lock()
	j.solo = append(j.solo, pid)
	j.mu.Unlock()
}

func (j *pgroupJob) Kill() error { return j.Signal(int(syscall.SIGKILL)) }

func (j *pgroupJob) Close() error { return nil }

func (j *pgroupJob) Mechanism() Mechanism { return ProcessGroup }

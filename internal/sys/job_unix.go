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
// Teardown is pid-based, so it carries the usual small pid-reuse window if a
// leader is reaped before Kill runs.
type pgroupJob struct {
	mu    sync.Mutex // guards pgids: Assign appends while Signal/Kill read
	pgids []int
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
	j.mu.Unlock()

	var firstErr error
	for _, pgid := range pgids {
		// Negative pid targets the whole process group. Two errno values mean the
		// group is effectively gone, both benign for an idempotent teardown of a
		// group we own:
		//   ESRCH — no such process group (every member already reaped).
		//   EPERM — on macOS/BSD, signalling a group whose only remaining members
		//           are unreaped zombies returns EPERM, not ESRCH; the processes
		//           are already dead. (Linux returns ESRCH or success here.)
		if err := syscall.Kill(-pgid, syscall.Signal(sig)); err != nil &&
			!errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (j *pgroupJob) Kill() error { return j.Signal(int(syscall.SIGKILL)) }

func (j *pgroupJob) Close() error { return nil }

func (j *pgroupJob) Mechanism() Mechanism { return ProcessGroup }

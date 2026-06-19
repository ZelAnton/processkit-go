//go:build unix

package sys

import (
	"errors"
	"os/exec"
	"syscall"
)

// pgroupJob contains a tree via a POSIX process group: the child is made a group
// leader (Setpgid, so pgid == its pid) and teardown is killpg over the group.
// This is the macOS/BSD mechanism and the Linux fallback. Weaker than a cgroup or
// Job Object — a child that calls setsid escapes the group. Teardown is pid-based,
// so it carries the usual small pid-reuse window if the leader is reaped before
// Kill runs.
type pgroupJob struct {
	pgid int
	set  bool
}

func newJob() Job { return &pgroupJob{} }

func (j *pgroupJob) Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// New process group with pgid == child pid; descendants inherit it.
	cmd.SysProcAttr.Setpgid = true
}

func (j *pgroupJob) Assign(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("sys: Assign called before the process started")
	}
	// With Setpgid and no Pgid set, the kernel makes the child its own group
	// leader, so the group id equals the child pid.
	j.pgid = cmd.Process.Pid
	j.set = true
	return nil
}

func (j *pgroupJob) Kill() error {
	if !j.set {
		return nil
	}
	// Negative pid targets the whole process group. ESRCH means the group is
	// already gone — success for an idempotent teardown.
	if err := syscall.Kill(-j.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (j *pgroupJob) Close() error { return nil }

func (j *pgroupJob) Mechanism() Mechanism { return ProcessGroup }

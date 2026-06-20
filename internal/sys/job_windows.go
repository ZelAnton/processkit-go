//go:build windows

package sys

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// JOBOBJECTINFOCLASS value for JOBOBJECT_EXTENDED_LIMIT_INFORMATION. Defined
// locally so we don't depend on whether x/sys/windows exports the class constant.
const jobObjectExtendedLimitInformation = 9

// winJob contains a set of trees in one Windows Job Object with
// KILL_ON_JOB_CLOSE. Each child is created suspended (Configure), assigned to the
// job, then resumed (Assign) — closing the race where a fast child forks a
// grandchild before it is inside the job. os/exec hides the primary thread
// handle, so the resume goes via ntdll's NtResumeProcess. The job holds many
// children (a shared group) or one (a private per-run job).
type winJob struct {
	mu     sync.Mutex // guards handle: Assign/Kill read it while Close nulls it
	handle windows.Handle
}

func newJob() (Job, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &winJob{handle: job}, nil
}

func (j *winJob) Configure(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	return nil
}

func (j *winJob) Assign(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("sys: Assign called before the process started")
	}
	pid := uint32(cmd.Process.Pid)

	j.mu.Lock()
	jobHandle := j.handle
	j.mu.Unlock()
	if jobHandle == windows.InvalidHandle || jobHandle == 0 {
		return errors.New("sys: job is closed")
	}

	// UncontainedChildGuard: between Start (suspended) and a successful assign this
	// child is an orphan nothing reaps — Go has no Drop. Until contained, any
	// failure must terminate just this child (not the shared group).
	contained := false
	defer func() {
		if !contained {
			if h, e := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); e == nil {
				_ = windows.TerminateProcess(h, 1)
				_ = windows.CloseHandle(h)
			}
		}
	}()

	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, pid)
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(ph)

	// A nested-job ACCESS_DENIED (this process already in a no-nest job, common on
	// CI runners) surfaces here — never a silent uncontained spawn.
	if err := windows.AssignProcessToJobObject(jobHandle, ph); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	contained = true // the job now owns the child; the guard no-ops.

	// Resume the suspended child by handle (os/exec hides the primary thread). If
	// this fails the child is stranded suspended — terminate just it (don't kill
	// the shared group) and report.
	if err := ntResumeProcess(ph); err != nil {
		_ = windows.TerminateProcess(ph, 1)
		return fmt.Errorf("resume process: %w", err)
	}
	return nil
}

// Signal is unsupported on Windows: a Job Object delivers no signals, only the
// terminate path. Use Kill.
func (j *winJob) Signal(sig int) error { return ErrUnsupported }

// Suspend and Resume are unsupported on Windows for now: a Job Object has no
// freeze, and a correct whole-tree pause needs a per-member thread-suspend walk
// (deferred). Honest ErrUnsupported rather than a partial pause.
func (j *winJob) Suspend() error { return ErrUnsupported }
func (j *winJob) Resume() error  { return ErrUnsupported }

// Adopt assigns an externally-started process to the job, so it (and everything it
// spawns from now on) is torn down with the group. A process that has already
// exited (and been reaped) is a benign success; a real failure — including a
// permission error, or a nested no-break job — is surfaced, never a silent no-op.
// The whole operation holds the job lock so it can't race a concurrent Close.
func (j *winJob) Adopt(pid int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle == windows.InvalidHandle || j.handle == 0 {
		return errors.New("sys: job is closed")
	}

	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		// A never-existed / already-reaped pid is benign; any other failure (e.g.
		// ACCESS_DENIED for a process we can't touch) is real.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(ph)

	if err := windows.AssignProcessToJobObject(j.handle, ph); err != nil {
		// If the process exited between OpenProcess and the assign, treat it as a
		// benign no-op; otherwise surface the failure.
		if exited, e := processExited(ph); e == nil && exited {
			return nil
		}
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// processExited reports whether the process (by handle) has terminated.
func processExited(h windows.Handle) (bool, error) {
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false, err
	}
	return code != stillActive, nil
}

// stillActive is STILL_ACTIVE (259): GetExitCodeProcess reports it while a process
// is running. (A process that genuinely exits with 259 is indistinguishable, an
// accepted Win32 quirk.)
const stillActive = 259

func (j *winJob) Kill() error {
	j.mu.Lock()
	h := j.handle
	j.mu.Unlock()
	if h == windows.InvalidHandle || h == 0 {
		return nil
	}
	return windows.TerminateJobObject(h, 1)
}

func (j *winJob) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.handle != windows.InvalidHandle && j.handle != 0 {
		err := windows.CloseHandle(j.handle)
		j.handle = windows.InvalidHandle
		return err
	}
	return nil
}

func (j *winJob) Mechanism() Mechanism { return JobObject }

var (
	ntdll               = windows.NewLazySystemDLL("ntdll.dll")
	procNtResumeProcess = ntdll.NewProc("NtResumeProcess")
)

// ntResumeProcess resumes every thread of the process (by handle), undoing the
// CREATE_SUSPENDED suspend in one call. NtResumeProcess is undocumented but has
// been stable since NT and is widely used for exactly this — resuming a process
// whose primary thread handle you don't hold. The handle must carry
// PROCESS_SUSPEND_RESUME.
func ntResumeProcess(h windows.Handle) error {
	r, _, _ := procNtResumeProcess.Call(uintptr(h))
	if r != 0 {
		return fmt.Errorf("NtResumeProcess returned NTSTATUS 0x%x", uint32(r))
	}
	return nil
}

//go:build windows

package sys

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// JOBOBJECTINFOCLASS value for JOBOBJECT_EXTENDED_LIMIT_INFORMATION. Defined
// locally so we don't depend on whether x/sys/windows exports the class constant.
const jobObjectExtendedLimitInformation = 9

// winJob contains a tree in a Windows Job Object with KILL_ON_JOB_CLOSE. The
// child is created suspended (Configure), assigned to the job, then resumed
// (Assign) — closing the race where a fast child forks a grandchild before it is
// inside the job. os/exec hides the primary thread handle, so the resume goes via
// a Toolhelp thread snapshot.
type winJob struct {
	handle   windows.Handle
	assigned bool
}

func newJob() Job { return &winJob{handle: windows.InvalidHandle} }

func (j *winJob) Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

func (j *winJob) Assign(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("sys: Assign called before the process started")
	}
	pid := uint32(cmd.Process.Pid)

	// UncontainedChildGuard: between Start (suspended) and a successful assign the
	// child is an orphan nothing reaps — Go has no Drop. Until contained, any
	// failure path must terminate it.
	contained := false
	defer func() {
		if !contained {
			if h, e := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); e == nil {
				_ = windows.TerminateProcess(h, 1)
				_ = windows.CloseHandle(h)
			}
		}
	}()

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}

	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, pid)
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(ph)

	// A nested-job ACCESS_DENIED (this process already in a no-nest job, common on
	// CI runners) surfaces here — never a silent uncontained spawn.
	if err := windows.AssignProcessToJobObject(job, ph); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	// From here the job owns the child: record it so even a resume failure is
	// torn down via Kill (the job), not left as an uncontained orphan.
	contained = true
	j.handle = job
	j.assigned = true

	// Resume the suspended child. os/exec hides the primary thread handle, so we
	// resume the whole process by handle via ntdll rather than enumerating its
	// threads with a Toolhelp snapshot (which races a just-created thread under
	// load). If this fails the child is contained but stranded — Kill (the job
	// owns it) reaps it, never a hang.
	if err := ntResumeProcess(ph); err != nil {
		return fmt.Errorf("resume process: %w", err)
	}
	return nil
}

func (j *winJob) Kill() error {
	if !j.assigned {
		return nil
	}
	return windows.TerminateJobObject(j.handle, 1)
}

func (j *winJob) Close() error {
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

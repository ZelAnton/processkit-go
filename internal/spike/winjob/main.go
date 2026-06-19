//go:build windows

// Spike W (Phase 0): prove a Windows Job Object with KILL_ON_JOB_CLOSE contains a
// whole process tree — including a grandchild forked *after* the
// CREATE_SUSPENDED → assign → resume handshake — and that the
// UncontainedChildGuard reaps the suspended child on the spawn→assign window.
//
// os/exec hides the child's primary thread handle, so the resume is reconstructed
// via a Toolhelp thread snapshot (the gap the ROADMAP's Phase 0 Windows spike must
// close). This is throwaway de-risk code; the real backend lands in internal/sys/.
//
// Run:  go run ./internal/spike/winjob
// Self-re-exec modes: default = parent, "child", "grandchild".
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// JOBOBJECTINFOCLASS value for JOBOBJECT_EXTENDED_LIMIT_INFORMATION. Defined
// locally so the spike doesn't depend on whether x/sys/windows exports the class
// constant (it reliably exports the struct + the LIMIT flag).
const jobObjectExtendedLimitInformation = 9

func main() {
	mode := "parent"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "child":
		child()
	case "grandchild":
		grandchild()
	default:
		os.Exit(parent())
	}
}

func fail(format string, a ...any) int {
	fmt.Printf("FAIL: "+format+"\n", a...)
	return 1
}

func parent() int {
	self, err := os.Executable()
	if err != nil {
		return fail("os.Executable: %v", err)
	}

	// 1) Job Object with kill-on-close: closing the last handle (or TerminateJobObject)
	//    reaps every process in the job, descendants included.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fail("CreateJobObject: %v", err)
	}
	defer windows.CloseHandle(job)

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fail("SetInformationJobObject: %v", err)
	}

	// 2) Spawn the child CREATE_SUSPENDED so it cannot fork a grandchild before it
	//    is inside the job. Capture its stdout to learn the grandchild pid.
	cmd := exec.Command(self, "child")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail("StdoutPipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fail("start child: %v", err)
	}
	childPid := uint32(cmd.Process.Pid)

	// 3) UncontainedChildGuard — THE key Windows correctness device. Until the
	//    child is contained, any early return must reap the suspended orphan
	//    (Go has no Drop to do it for us).
	contained := false
	defer func() {
		if !contained {
			fmt.Printf("guard: terminating uncontained suspended child %d\n", childPid)
			if h, e := windows.OpenProcess(windows.PROCESS_TERMINATE, false, childPid); e == nil {
				_ = windows.TerminateProcess(h, 1)
				_ = windows.CloseHandle(h)
			}
		}
	}()

	// 4) Assign the child to the job. A nested-job ERROR_ACCESS_DENIED (this process
	//    already in a no-nest job, common on CI runners) surfaces here — never a
	//    silent uncontained spawn.
	ch, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, childPid)
	if err != nil {
		return fail("OpenProcess child: %v", err)
	}
	defer windows.CloseHandle(ch)
	if err := windows.AssignProcessToJobObject(job, ch); err != nil {
		return fail("AssignProcessToJobObject (nested-job ACCESS_DENIED is the expected CI failure): %v", err)
	}
	contained = true // the job now owns the child; the guard no-ops.

	// 5) Resume the primary thread. os/exec hides the thread handle, so walk a
	//    Toolhelp snapshot for threads owned by the child pid and resume them.
	resumed, err := resumeProcessThreads(childPid)
	if err != nil {
		return fail("resume snapshot: %v", err)
	}
	if resumed == 0 {
		return fail("resume found 0 threads — stranded-suspended (must error+reap, not hang)")
	}

	// 6) The child (now running, inside the job) forks a grandchild and reports its pid.
	gpid, err := readGrandchildPid(stdout)
	if err != nil {
		return fail("read grandchild pid: %v", err)
	}

	// 7) Hold a handle to the grandchild BEFORE teardown so we can prove its death
	//    without pid-reuse ambiguity (wait on the handle, not re-probe the pid).
	gh, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, gpid)
	if err != nil {
		return fail("OpenProcess grandchild %d (should be alive & in the job): %v", gpid, err)
	}
	defer windows.CloseHandle(gh)
	fmt.Printf("contained: child=%d grandchild=%d (resumed %d thread(s))\n", childPid, gpid, resumed)

	// 8) Tear down the whole tree via the job.
	if err := windows.TerminateJobObject(job, 1); err != nil {
		return fail("TerminateJobObject: %v", err)
	}

	// 9) Prove the grandchild was reaped by the job teardown.
	s, err := windows.WaitForSingleObject(gh, 5000)
	if err != nil {
		return fail("WaitForSingleObject grandchild: %v", err)
	}
	if s != windows.WAIT_OBJECT_0 {
		return fail("grandchild still alive 5s after TerminateJobObject (wait state=0x%x)", s)
	}
	fmt.Println("PASS: grandchild reaped by Job Object teardown — whole tree contained, assignment-window race closed")
	return 0
}

// resumeProcessThreads resumes every thread owned by pid via a Toolhelp snapshot.
// A CREATE_SUSPENDED child has exactly one (primary) thread; returns the count.
func resumeProcessThreads(pid uint32) (int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)

	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, e := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if e != nil {
			continue
		}
		_, _ = windows.ResumeThread(th)
		_ = windows.CloseHandle(th)
		resumed++
	}
	return resumed, nil
}

func readGrandchildPid(stdout interface{ Read([]byte) (int, error) }) (uint32, error) {
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("read line %q: %w", line, err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "grandchild=")), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", line, err)
	}
	return uint32(v), nil
}

// child runs inside the job (after resume) and forks a grandchild WITHOUT a
// breakaway flag, so the grandchild auto-joins the job.
func child() {
	self, _ := os.Executable()
	g := exec.Command(self, "grandchild")
	if err := g.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "child: start grandchild:", err)
		os.Exit(1)
	}
	fmt.Printf("grandchild=%d\n", g.Process.Pid)
	time.Sleep(60 * time.Second) // stay alive so the parent can tear us down via the job
}

func grandchild() {
	time.Sleep(120 * time.Second) // live until the job kills us
}

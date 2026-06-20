//go:build windows

package sys

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// filetimeTo100ns reads a FILETIME holding a *duration* (not an absolute time) as a
// count of 100-ns ticks.
func filetimeTo100ns(ft windows.Filetime) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS; only PeakWorkingSetSize is
// read (the per-process peak resident set).
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// K32GetProcessMemoryInfo (kernel32, Windows 7+) works with the limited query right
// we open below, unlike the psapi entry point.
var procGetProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// processMetrics reads a single process's CPU time and peak working set by pid.
func processMetrics(pid int) ProcMetrics {
	var m ProcMetrics
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return m // the process is gone
	}
	defer windows.CloseHandle(h)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err == nil {
		// kernel/user are *durations* in 100-ns units — NOT absolute times, so use
		// the raw ticks (Filetime.Nanoseconds would wrongly subtract the 1601 epoch).
		ticks := filetimeTo100ns(kernel) + filetimeTo100ns(user)
		m.CPUTime, m.HasCPU = nanosFromUnit(ticks, 100), true
	}

	counters := processMemoryCounters{}
	counters.cb = uint32(unsafe.Sizeof(counters))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(counters.cb)); r != 0 {
		m.PeakMemory, m.HasMem = uint64(counters.PeakWorkingSetSize), true
	}
	return m
}

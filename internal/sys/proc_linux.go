//go:build linux

package sys

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// userHZ is the clock-ticks-per-second unit that /proc/<pid>/stat reports CPU time
// in. It is fixed at 100 on Linux (USER_HZ), independent of the kernel's CONFIG_HZ,
// so we don't need cgo/sysconf to read _SC_CLK_TCK.
const userHZ = 100

// processMetrics reads a single process's CPU time and peak RSS from /proc.
func processMetrics(pid int) ProcMetrics {
	var m ProcMetrics
	if cpu, ok := procCPUTime(pid); ok {
		m.CPUTime, m.HasCPU = cpu, true
	}
	if mem, ok := procPeakRSS(pid); ok {
		m.PeakMemory, m.HasMem = mem, true
	}
	return m
}

// procCPUTime sums utime+stime from /proc/<pid>/stat and converts ticks to a
// duration. The comm field (field 2) is parenthesised and may contain spaces and
// parens, so parsing starts after the LAST ')'.
func procCPUTime(pid int) (time.Duration, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 {
		return 0, false
	}
	// After ')' the fields are state, ppid, …; utime is field 14 and stime field 15
	// overall, i.e. indices 11 and 12 in this tail (index 0 == state).
	fields := strings.Fields(s[rparen+1:])
	if len(fields) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseInt(fields[11], 10, 64)
	stime, err2 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil || utime < 0 || stime < 0 {
		return 0, false
	}
	ticks := utime + stime
	if ticks < 0 { // overflow
		ticks = int64(^uint64(0) >> 1)
	}
	// ticks → nanoseconds: ticks * (1e9 / userHZ) ns per tick.
	return nanosFromUnit(ticks, 1_000_000_000/userHZ), true
}

// procPeakRSS reads VmHWM (peak resident set, in kB) from /proc/<pid>/status.
func procPeakRSS(pid int) (uint64, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, found := strings.CutPrefix(line, "VmHWM:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return saturatingMulU64(kb, 1024), true
	}
	return 0, false
}

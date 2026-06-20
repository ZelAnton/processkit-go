//go:build unix && !linux

package sys

// processMetrics is unavailable on macOS / the BSDs (no /proc, and the per-process
// task-info syscalls are not wired here): every metric is reported as unavailable.
func processMetrics(pid int) ProcMetrics { return ProcMetrics{} }

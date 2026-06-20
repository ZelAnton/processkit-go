package processkit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ZelAnton/processkit-go/internal/sys"
)

// GroupStats is a whole-tree resource snapshot from [Group.Stats]. CPU time and
// peak memory are reported only by the Job Object backend (Windows); the POSIX
// process-group backend reports the active count only (no kernel accumulator
// without a cgroup), so their accessors return false there.
type GroupStats struct {
	activeProcesses int
	cpuTime         time.Duration
	hasCPU          bool
	peakMemory      uint64
	hasMem          bool
}

// ActiveProcesses returns the number of live processes (or contained process
// groups, on the POSIX backend) in the group.
func (s GroupStats) ActiveProcesses() int { return s.activeProcesses }

// CPUTime returns the group's cumulative CPU time (user+system) and whether the
// backend could read it.
func (s GroupStats) CPUTime() (time.Duration, bool) { return s.cpuTime, s.hasCPU }

// PeakMemoryBytes returns the group's peak memory and whether the backend could
// read it. The figure is not comparable across platforms (Windows reports peak
// committed memory) and is not the sum of per-process peaks.
func (s GroupStats) PeakMemoryBytes() (uint64, bool) { return s.peakMemory, s.hasMem }

// RunProfile summarises one run's resource usage, sampled over its lifetime by
// [RunningProcess.Profile].
type RunProfile struct {
	exitCode    int
	hasExitCode bool
	duration    time.Duration
	cpuTime     time.Duration
	hasCPU      bool
	peakMemory  uint64
	hasMem      bool
	samples     int
}

// ExitCode returns the run's exit code and true, or (0, false) if it was killed by
// a timeout or signal.
func (p RunProfile) ExitCode() (int, bool) { return p.exitCode, p.hasExitCode }

// Duration returns the wall-clock time from start to exit.
func (p RunProfile) Duration() time.Duration { return p.duration }

// CPUTime returns the cumulative CPU time at the last sample, and whether any CPU
// sample succeeded.
func (p RunProfile) CPUTime() (time.Duration, bool) { return p.cpuTime, p.hasCPU }

// PeakMemoryBytes returns the maximum peak-RSS observed across samples.
func (p RunProfile) PeakMemoryBytes() (uint64, bool) { return p.peakMemory, p.hasMem }

// Samples returns how many times the run was sampled.
func (p RunProfile) Samples() int { return p.samples }

// AvgCPU returns the average CPU usage in cores (CPU time / wall duration); ok is
// false when there was no CPU sample or the duration was zero. It can exceed 1.0
// for a multi-threaded process.
func (p RunProfile) AvgCPU() (float64, bool) {
	if !p.hasCPU || p.duration <= 0 {
		return 0, false
	}
	return p.cpuTime.Seconds() / p.duration.Seconds(), true
}

// Stats returns a whole-tree resource snapshot of the group.
func (g *Group) Stats() (GroupStats, error) {
	st, err := g.job.Stats()
	if err != nil {
		return GroupStats{}, fmt.Errorf("processkit: reading group stats: %w", err)
	}
	return GroupStats{
		activeProcesses: st.ActiveProcesses,
		cpuTime:         st.CPUTime,
		hasCPU:          st.HasCPU,
		peakMemory:      st.PeakMemoryBytes,
		hasMem:          st.HasMem,
	}, nil
}

// SampleStats returns a channel of resource snapshots sampled every interval
// (clamped to a 1ms minimum), starting with an immediate sample. The channel is
// closed when ctx is cancelled or a sample fails. Drain it until it closes, or
// cancel ctx, so the sampler doesn't block on a slow reader.
func (g *Group) SampleStats(ctx context.Context, interval time.Duration) <-chan GroupStats {
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	ch := make(chan GroupStats)
	go func() {
		defer close(ch)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			st, err := g.Stats()
			if err != nil {
				return
			}
			select {
			case ch <- st:
			case <-ctx.Done():
				return
			}
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// CPUTime returns this process's cumulative CPU time (user+system) and whether it
// could be read — available on Linux and Windows, not on macOS / the BSDs. It is
// unavailable once the process has exited (its pid may have been recycled).
func (p *RunningProcess) CPUTime() (time.Duration, bool) {
	if p.exited() {
		return 0, false
	}
	m := sys.ProcessMetrics(p.Pid())
	return m.CPUTime, m.HasCPU
}

// PeakMemoryBytes returns this process's peak resident memory and whether it could
// be read — available on Linux and Windows, not on macOS / the BSDs. It is
// unavailable once the process has exited (its pid may have been recycled).
func (p *RunningProcess) PeakMemoryBytes() (uint64, bool) {
	if p.exited() {
		return 0, false
	}
	m := sys.ProcessMetrics(p.Pid())
	return m.PeakMemory, m.HasMem
}

// Elapsed returns how long the process has been running since it was started.
func (p *RunningProcess) Elapsed() time.Duration { return time.Since(p.startTime) }

// Profile waits for the process to exit, sampling its CPU time and peak memory
// every interval (clamped to a 1ms minimum), and returns a [RunProfile]. Like
// [RunningProcess.Wait] it returns ctx's bare error if ctx is done first (the
// process keeps running). CPU and memory are unavailable on macOS / the BSDs, so a
// profile there has only the duration, exit code, and sample count.
func (p *RunningProcess) Profile(ctx context.Context, interval time.Duration) (RunProfile, error) {
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	pid := p.Pid()

	var (
		mu      sync.Mutex
		cpu     time.Duration
		hasCPU  bool
		peak    uint64
		hasMem  bool
		samples int
	)
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			mu.Lock()
			samples++
			mu.Unlock()
			// Guard against reading a reaped (possibly recycled) pid: skip the read
			// if the process has exited just before or during it.
			if p.exited() {
				continue
			}
			m := sys.ProcessMetrics(pid)
			if p.exited() {
				continue
			}
			mu.Lock()
			if m.HasCPU {
				cpu, hasCPU = m.CPUTime, true
			}
			if m.HasMem && m.PeakMemory > peak {
				peak, hasMem = m.PeakMemory, true
			}
			mu.Unlock()
		}
	}()

	out, werr := p.Wait(ctx)
	duration := time.Since(p.startTime)
	<-sampleDone

	mu.Lock()
	defer mu.Unlock()
	prof := RunProfile{
		duration:   duration,
		cpuTime:    cpu,
		hasCPU:     hasCPU,
		peakMemory: peak,
		hasMem:     hasMem,
		samples:    samples,
	}
	if code, ok := out.Code(); ok {
		prof.exitCode, prof.hasExitCode = code, true
	}
	return prof, werr
}

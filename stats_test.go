package processkit

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestRunProfile_AvgCPU(t *testing.T) {
	// 2s of CPU over a 1s wall duration → 2 cores.
	p := RunProfile{cpuTime: 2 * time.Second, hasCPU: true, duration: time.Second}
	if avg, ok := p.AvgCPU(); !ok || avg != 2.0 {
		t.Fatalf("AvgCPU = %v, %v; want 2.0, true", avg, ok)
	}
	// No CPU sample → unavailable.
	if _, ok := (RunProfile{duration: time.Second}).AvgCPU(); ok {
		t.Fatal("AvgCPU should be unavailable with no CPU sample")
	}
	// Zero duration → unavailable (no divide-by-zero).
	if _, ok := (RunProfile{cpuTime: time.Second, hasCPU: true}).AvgCPU(); ok {
		t.Fatal("AvgCPU should be unavailable with zero duration")
	}
}

func TestGroup_Stats(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	for i := 0; i < 2; i++ {
		if _, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...)); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond) // let both be running

	st, err := g.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.ActiveProcesses() < 2 {
		t.Fatalf("ActiveProcesses = %d, want >= 2", st.ActiveProcesses())
	}
	// The Job Object backend reports CPU + peak memory; the process-group backend
	// reports the count only.
	_, hasCPU := st.CPUTime()
	_, hasMem := st.PeakMemoryBytes()
	if runtime.GOOS == "windows" {
		if !hasCPU || !hasMem {
			t.Errorf("Windows group stats should have CPU+memory (hasCPU=%v hasMem=%v)", hasCPU, hasMem)
		}
	} else if hasCPU || hasMem {
		t.Errorf("the process-group backend should report no CPU/memory (hasCPU=%v hasMem=%v)", hasCPU, hasMem)
	}
}

func TestRunningProcess_Profile(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("per-process metrics are unavailable on macOS")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	p, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("burncpu", "PK_BURN_MS=300")...))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	prof, err := p.Profile(ctx, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if c, ok := prof.ExitCode(); !ok || c != 0 {
		t.Fatalf("exit = %d (ok=%v), want 0", c, ok)
	}
	if prof.Samples() == 0 {
		t.Error("expected at least one sample over a 300ms run sampled every 20ms")
	}
	if prof.Duration() <= 0 {
		t.Error("duration should be positive")
	}
	if cpu, ok := prof.CPUTime(); !ok || cpu <= 0 {
		t.Errorf("CPUTime = %v (ok=%v), want > 0 (the helper burns CPU)", cpu, ok)
	}
	if avg, ok := prof.AvgCPU(); !ok || avg <= 0 {
		t.Errorf("AvgCPU = %v (ok=%v), want > 0", avg, ok)
	}
}

func TestGroup_SampleStats(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()
	if _, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sctx, cancel := context.WithCancel(ctx)
	ch := g.SampleStats(sctx, 20*time.Millisecond)

	for i := 0; i < 3; i++ { // collect a few snapshots
		select {
		case st, ok := <-ch:
			if !ok {
				t.Fatal("sampler channel closed before yielding 3 snapshots")
			}
			if st.ActiveProcesses() < 1 {
				t.Fatalf("snapshot active = %d, want >= 1", st.ActiveProcesses())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no snapshot within 2s")
		}
	}

	cancel() // stop sampling
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed after cancel — success
			}
		case <-deadline:
			t.Fatal("sampler channel did not close after cancel")
		}
	}
}

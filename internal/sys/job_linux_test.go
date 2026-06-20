//go:build linux

package sys

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- pure helpers (run anywhere) ---

func TestControllersToEnable(t *testing.T) {
	cases := []struct {
		needed  []string
		subtree string
		want    []string
	}{
		{[]string{"memory", "pids"}, "cpu memory pids", nil},                   // all already enabled
		{[]string{"memory", "pids", "cpu"}, "memory", []string{"pids", "cpu"}}, // order preserved
		{[]string{"memory"}, "", []string{"memory"}},                           // empty subtree → all needed
		{[]string{"pids"}, "pids io hugetlb", nil},                             // extra controllers ignored
	}
	for _, c := range cases {
		got := controllersToEnable(c.needed, c.subtree)
		if !slices.Equal(got, c.want) {
			t.Errorf("controllersToEnable(%v, %q) = %v, want %v", c.needed, c.subtree, got, c.want)
		}
	}
}

func TestCPUMaxValue(t *testing.T) {
	cases := []struct {
		cores float64
		want  string
	}{
		{0.5, "50000 100000"},
		{2.0, "200000 100000"},
		{1.0, "100000 100000"},
		{0.00001, "1 100000"},                // rounds toward 0 but floors at 1
		{1e15, "4611686018427387904 100000"}, // absurd-but-finite quota clamps (2^62), no overflow
	}
	for _, c := range cases {
		if got := cpuMaxValue(c.cores); got != c.want {
			t.Errorf("cpuMaxValue(%v) = %q, want %q", c.cores, got, c.want)
		}
	}
}

func TestSelfCgroupRel(t *testing.T) {
	// On a v2 host /proc/self/cgroup is "0::<path>"; just confirm we parse a path.
	rel, err := selfCgroupRel()
	if err != nil {
		t.Skipf("no /proc/self/cgroup: %v", err)
	}
	if !strings.HasPrefix(rel, "/") {
		t.Errorf("selfCgroupRel = %q, want a path starting with /", rel)
	}
}

func TestCgroupNameSaltStable(t *testing.T) {
	first, second := cgroupNameSalt(), cgroupNameSalt()
	if first != second {
		t.Errorf("cgroupNameSalt must be stable within a process: %q vs %q", first, second)
	}
}

func TestKernelAtLeast(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{"5.15.0-91-generic", true},
		{"5.7.0", true},
		{"5.6.19", false},
		{"5.4.0-100", false},
		{"6.1.0", true},
		{"4.19.0", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := kernelAtLeast(c.rel, 5, 7); got != c.want {
			t.Errorf("kernelAtLeast(%q, 5, 7) = %v, want %v", c.rel, got, c.want)
		}
	}
}

// --- gated integration: needs a writable, delegated cgroup (privileged) ---

// newCgroupOrSkip creates a no-limit cgroup job or skips when the environment can't
// (read-only /sys/fs/cgroup, no delegation — the common CI case → process-group
// fallback). The fallback path is covered by the package's main suite.
func newCgroupOrSkip(t *testing.T) *cgroupJob {
	t.Helper()
	cg, err := createCgroup(Limits{})
	if err != nil {
		t.Skipf("no writable cgroup here (process-group fallback path): %v", err)
	}
	return cg
}

func TestCgroup_ContainAndKill(t *testing.T) {
	cg := newCgroupOrSkip(t)
	defer cg.Close()
	if cg.Mechanism() != CgroupV2 {
		t.Fatalf("mechanism = %v, want CgroupV2", cg.Mechanism())
	}

	cmd := exec.Command("sleep", "30")
	if err := cg.Configure(cmd); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cg.Assign(cmd); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// The child is now a member.
	if !slices.Contains(cg.members(), cmd.Process.Pid) {
		t.Fatalf("pid %d not in cgroup.procs %v", cmd.Process.Pid, cg.members())
	}

	// cgroup.kill tears the subtree down.
	if err := cg.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	go cmd.Wait() // reap whenever it dies
	deadline := time.Now().Add(3 * time.Second)
	for len(cg.members()) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("cgroup did not drain after Kill: %v", cg.members())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCgroup_FreezeResume(t *testing.T) {
	cg := newCgroupOrSkip(t)
	defer cg.Close()
	cmd := exec.Command("sleep", "30")
	_ = cg.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cg.Kill(); go cmd.Wait() }()
	if err := cg.Assign(cmd); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := cg.Suspend(); err != nil {
		t.Errorf("Suspend: %v", err)
	}
	if err := cg.Resume(); err != nil {
		t.Errorf("Resume: %v", err)
	}
}

// A memory cap either applies (we read memory.max back) or fails fast — never a
// silently-uncapped cgroup. Mirrors the crate's per-platform conformance test.
func TestCgroup_MemoryLimitAppliesOrFailsFast(t *testing.T) {
	// Skip if even a no-limit cgroup can't be made (no writable cgroup at all).
	if _, err := createCgroup(Limits{}); err != nil {
		t.Skipf("no writable cgroup here: %v", err)
	}
	cg, err := createCgroup(Limits{MemoryMax: 64 << 20, HasMemoryMax: true})
	if err != nil {
		// Fail-fast: the controllers couldn't be enabled (not the real cgroup root —
		// a container/systemd scope). This is the honest, expected path off bare metal.
		t.Logf("memory cap fail-fast (expected off the real cgroup root): %v", err)
		return
	}
	defer cg.Close()
	got, rerr := os.ReadFile(filepath.Join(cg.path, "memory.max"))
	if rerr != nil {
		t.Fatalf("read memory.max: %v", rerr)
	}
	if strings.TrimSpace(string(got)) != "67108864" {
		t.Errorf("memory.max = %q, want 67108864", got)
	}
}

package processkit

import (
	"context"
	"errors"
	"math"
	"runtime"
	"testing"
)

// Validation runs before any OS call, so it is hermetic and identical on every
// platform: a nonsensical cap is rejected as a *ResourceLimitError (ErrResourceLimit).
func TestNewGroup_ValidationRejectsNonsense(t *testing.T) {
	cases := []struct {
		name string
		opt  GroupOption
	}{
		{"zero memory", WithMemoryMax(0)},
		{"zero processes", WithMaxProcesses(0)},
		{"zero cpu", WithCPUQuota(0)},
		{"negative cpu", WithCPUQuota(-1)},
		{"NaN cpu", WithCPUQuota(math.NaN())},
		{"infinite cpu", WithCPUQuota(math.Inf(1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := NewGroup(tc.opt)
			if g != nil {
				g.Close()
				t.Fatal("a rejected limit must not return a group")
			}
			if !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("err = %v, want ErrResourceLimit", err)
			}
			var rle *ResourceLimitError
			if !errors.As(err, &rle) {
				t.Fatalf("err = %v, want a *ResourceLimitError", err)
			}
		})
	}
}

// A valid cap must either be honoured by a real container (a Windows Job Object) or
// fail fast with ErrResourceLimit — never a silently-unbounded group. This mirrors
// the crate's per-platform conformance test.
func TestNewGroup_LimitPlatformContract(t *testing.T) {
	g, err := NewGroup(WithMemoryMax(64 * 1024 * 1024))
	if g != nil {
		defer g.Close()
	}
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf("Windows Job Objects enforce a memory cap: %v", err)
		}
		if g.Mechanism() != MechanismJobObject {
			t.Fatalf("mechanism = %v, want JobObject", g.Mechanism())
		}
		return
	}
	// Every Unix backend here is a process group, which has no whole-tree cap, so a
	// limit must be rejected rather than silently dropped.
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("a limit on a container-less mechanism must be rejected; err = %v", err)
	}
}

// A no-limit NewGroup is unaffected — the surface stays backward-compatible.
func TestNewGroup_NoLimitsEverywhere(t *testing.T) {
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup() with no options: %v", err)
	}
	g.Close()
}

// On Windows, ActiveProcessLimit rejects the process that would exceed the cap: a
// single-process group with max_processes(1) admits the first start and refuses
// the second.
func TestNewGroup_MaxProcesses_Windows(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	if runtime.GOOS != "windows" {
		t.Skip("the active-process cap is a Windows Job Object feature")
	}
	ctx := context.Background()
	g, err := NewGroup(WithMaxProcesses(1))
	if err != nil {
		t.Fatalf("NewGroup(WithMaxProcesses(1)): %v", err)
	}
	defer g.Close()
	if g.Mechanism() != MechanismJobObject {
		t.Fatalf("mechanism = %v, want JobObject", g.Mechanism())
	}

	if _, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...)); err != nil {
		t.Fatalf("first child should fit the cap: %v", err)
	}
	if _, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...)); err == nil {
		t.Fatal("a second process must not be admitted past max_processes(1)")
	}
}

// On Windows, a generous memory cap plus a half-core CPU cap must be accepted (both
// SetInformationJobObject calls succeed) and must not break an ordinary child.
func TestNewGroup_MemoryAndCPU_Windows(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	if runtime.GOOS != "windows" {
		t.Skip("Job Object memory + CPU caps are a Windows feature")
	}
	ctx := context.Background()
	g, err := NewGroup(WithMemoryMax(512*1024*1024), WithCPUQuota(0.5))
	if err != nil {
		t.Fatalf("create capped group: %v", err)
	}
	defer g.Close()

	p, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_STDOUT=hi", "PK_CODE=0")...))
	if err != nil {
		t.Fatalf("spawn small child: %v", err)
	}
	out, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if c, ok := out.Code(); !ok || c != 0 {
		t.Fatalf("exit = %d (ok=%v), want 0", c, ok)
	}
}

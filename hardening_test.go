package processkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartError_MatchesErrStart(t *testing.T) {
	err := error(&StartError{Program: "git", Err: errors.New("boom")})
	if !errors.Is(err, ErrStart) {
		t.Error("a *StartError should match errors.Is(err, ErrStart)")
	}
	// It still unwraps to the cause.
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Error() should carry the cause: %v", err)
	}
}

func TestCmd_AppendEnv(t *testing.T) {
	// On a fresh command, AppendEnv inherits the process environment and adds.
	t.Setenv("PK_INHERITED", "yes")
	inv := Command("tool").AppendEnv("PK_EXTRA=1").invocation()
	if !slices.Contains(inv.Env, "PK_EXTRA=1") {
		t.Error("AppendEnv should add the entry")
	}
	if !slices.Contains(inv.Env, "PK_INHERITED=yes") {
		t.Error("AppendEnv on a fresh command should inherit the process environment")
	}

	// After WithEnv (which replaces), AppendEnv builds on that set, not the inherited one.
	inv2 := Command("tool").WithEnv("A=1").AppendEnv("B=2").invocation()
	if !slices.Contains(inv2.Env, "A=1") || !slices.Contains(inv2.Env, "B=2") {
		t.Errorf("AppendEnv after WithEnv should keep both: %v", inv2.Env)
	}
	if slices.Contains(inv2.Env, "PK_INHERITED=yes") {
		t.Error("AppendEnv after WithEnv must not re-inherit the process environment")
	}
}

func TestCmd_AppendEnv_DoesNotMutateReceiver(t *testing.T) {
	base := Command("tool").WithEnv("A=1")
	_ = base.AppendEnv("B=2")
	if slices.Contains(base.invocation().Env, "B=2") {
		t.Error("AppendEnv must not mutate the receiver (copy-on-write)")
	}
}

// The zero-value Signal must be rejected, not silently delivered as SIGTERM.
func TestGroup_SignalZeroValueRejected(t *testing.T) {
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()
	if err := g.Signal(Signal{}); err == nil {
		t.Error("Group.Signal(Signal{}) should reject the unspecified zero Signal")
	}
	if err := g.Signal(SignalKill); err != nil { // a curated signal still works
		t.Errorf("SignalKill should work: %v", err)
	}
}

func TestGroup_Processes(t *testing.T) {
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
	if got := len(g.Processes()); got != 2 {
		t.Fatalf("Processes() = %d live handles, want 2", got)
	}
	// The handles carry the pids.
	if pids := memberPids(g); len(pids) != 2 {
		t.Errorf("memberPids = %v, want 2 pids", pids)
	}
}

// A defer Close() during a panic still reaps the tree — the no-orphan guarantee
// holds on the unwinding path (the Windows Job Object enforces it; Unix relies on
// the deferred Close running, which it does during a panic).
func TestGroup_CloseReapsOnPanicPath(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	var pid int
	func() {
		defer func() { _ = recover() }()
		g, err := NewGroup()
		if err != nil {
			t.Fatalf("NewGroup: %v", err)
		}
		defer g.Close()
		p, err := g.Start(context.Background(), Command(selfExe(t)).WithEnv(helperEnv("sleep")...))
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		pid = p.Pid()
		panic("simulated failure mid-use")
	}()
	// After the panic unwound through defer Close(), the child must be gone.
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive after a panic-path Close()", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Many concurrent Start/Close cycles must stay race-clean and leak nothing (run
// under -race in CI).
func TestGroup_ConcurrentStartCloseStress(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := NewGroup()
			if err != nil {
				t.Errorf("NewGroup: %v", err)
				return
			}
			for j := 0; j < 3; j++ {
				if _, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...)); err != nil {
					t.Errorf("Start: %v", err)
				}
			}
			if err := g.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()
}

// Start racing Close on ONE group must never orphan a grandchild: a child started
// here is either torn down by the Close that snapshots it, or refused before it
// spawns. Exercises the fd/drain window the cgroup backend has (run under -race and,
// privileged, the cgroup backend).
func TestGroup_StartRacingClose(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	dir := t.TempDir()
	for iter := 0; iter < 15; iter++ {
		g, err := NewGroup()
		if err != nil {
			t.Fatalf("NewGroup: %v", err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			pidfile := filepath.Join(dir, fmt.Sprintf("%d-%d.pid", iter, i))
			go func() {
				defer wg.Done()
				_, _ = g.Start(context.Background(),
					Command(selfExe(t)).WithEnv(helperEnv("groupchild", "PK_PIDFILE="+pidfile)...))
			}()
		}
		time.Sleep(time.Duration(iter%3) * time.Millisecond) // vary the race window
		_ = g.Close()
		wg.Wait()
		_ = g.Close() // idempotent
	}

	// Any grandchild that was actually spawned (wrote its pidfile) must be dead.
	entries, _ := os.ReadDir(dir)
	deadline := time.Now().Add(3 * time.Second)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			continue
		}
		for processAlive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("grandchild %d (from %s) still alive — orphan after Start-racing-Close", pid, e.Name())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// Adopt racing Close on one group must not leave the adopted process running: if
// Adopt won (no error), it joined the container before Close snapshotted, so Close
// killed it; if it lost (errGroupClosed), it was never pulled in. Exercises the
// Adopt-under-g.mu fix under -race (privileged: the cgroup fd/dir race window).
func TestGroup_AdoptRacingClose(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	for iter := 0; iter < 15; iter++ {
		ext := exec.Command(selfExe(t))
		ext.Env = helperEnv("sleep")
		if err := ext.Start(); err != nil {
			t.Fatalf("start external process: %v", err)
		}
		reaped := make(chan struct{})
		go func() { _ = ext.Wait(); close(reaped) }() // reap so the dead process isn't a zombie

		g, err := NewGroup()
		if err != nil {
			t.Fatalf("NewGroup: %v", err)
		}
		var adoptErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); adoptErr = g.Adopt(ext.Process) }()
		time.Sleep(time.Duration(iter%2) * time.Millisecond) // vary the race window
		_ = g.Close()
		wg.Wait()

		if adoptErr == nil {
			// Adopt joined it to the group, so Close must have killed it.
			select {
			case <-reaped:
			case <-time.After(3 * time.Second):
				_ = ext.Process.Kill()
				<-reaped
				t.Fatalf("iter %d: adopted process survived Close — orphan", iter)
			}
		} else {
			// Adopt lost the race (errGroupClosed): the process is ours to kill.
			_ = ext.Process.Kill()
			<-reaped
		}
	}
}

// Guard the AppendEnv inherit path against an empty process env oddity.
func TestCmd_AppendEnv_EmptyAdditions(t *testing.T) {
	n := len(os.Environ())
	inv := Command("tool").AppendEnv().invocation() // no additions: still inherits
	if len(inv.Env) != n {
		t.Errorf("AppendEnv() with no entries should equal the inherited env (%d), got %d", n, len(inv.Env))
	}
}

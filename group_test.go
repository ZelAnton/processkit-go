package processkit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGroup_CloseReapsGrandchild is the shared-group kill-on-drop contract: a
// grandchild spawned by a group member must be reaped when the group is closed.
func TestGroup_CloseReapsGrandchild(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess / kill-on-drop test")
	}
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	pidfile := filepath.Join(t.TempDir(), "gpid")
	_, err = g.Start(context.Background(),
		Command(selfExe(t)).WithEnv(helperEnv("groupchild", "PK_PIDFILE="+pidfile)...))
	if err != nil {
		_ = g.Close()
		t.Fatalf("Start: %v", err)
	}
	pid := waitPidFile(t, pidfile)

	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive after Group.Close — orphan leak (mechanism=%v)", pid, g.Mechanism())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestGroup_WaitAny(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	slow, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...))
	if err != nil {
		t.Fatalf("Start slow: %v", err)
	}
	fast, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=7")...))
	if err != nil {
		t.Fatalf("Start fast: %v", err)
	}

	i, outcome, err := WaitAny(ctx, slow, fast)
	if err != nil {
		t.Fatalf("WaitAny: %v", err)
	}
	if i != 1 {
		t.Fatalf("WaitAny index = %d, want 1 (the fast process)", i)
	}
	if c, ok := outcome.Code(); !ok || c != 7 {
		t.Fatalf("fast outcome = %v, want exited(7)", outcome)
	}
}

func TestGroup_WaitAll(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	a, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=0")...))
	if err != nil {
		t.Fatalf("Start a: %v", err)
	}
	b, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=3")...))
	if err != nil {
		t.Fatalf("Start b: %v", err)
	}

	outs, err := WaitAll(ctx, a, b)
	if err != nil {
		t.Fatalf("WaitAll: %v", err)
	}
	if c, ok := outs[0].Code(); !ok || c != 0 {
		t.Fatalf("a outcome = %v, want exited(0)", outs[0])
	}
	if c, ok := outs[1].Code(); !ok || c != 3 {
		t.Fatalf("b outcome = %v, want exited(3)", outs[1])
	}
}

func TestOutputAll(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	cmds := []*Cmd{
		Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=0", "PK_STDOUT=a")...),
		Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=1", "PK_STDOUT=b")...),
		Command("processkit-no-such-program-xyz"),
	}
	results := OutputAll(ctx, cmds, 2)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	// 0: clean success.
	if results[0].Err != nil || results[0].Result.Stdout() != "a" {
		t.Fatalf("results[0] = %+v", results[0])
	}
	// 1: a non-zero exit is data (in Result), not a batch error.
	if results[1].Err != nil {
		t.Fatalf("non-zero exit should be in Result, not Err: %v", results[1].Err)
	}
	if c, ok := results[1].Result.Code(); !ok || c != 1 {
		t.Fatalf("results[1] code = %v, want 1", results[1].Result.Outcome())
	}
	// 2: a real spawn failure is an Err.
	if !errors.Is(results[2].Err, ErrNotFound) {
		t.Fatalf("results[2].Err = %v, want ErrNotFound", results[2].Err)
	}
}

func TestGroup_Members(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	live, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...))
	if err != nil {
		t.Fatalf("Start live: %v", err)
	}
	gone, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=0")...))
	if err != nil {
		t.Fatalf("Start gone: %v", err)
	}

	if !containsPid(g.Members(), live.Pid()) {
		t.Fatalf("live process %d not in Members %v", live.Pid(), g.Members())
	}
	if _, err := gone.Wait(ctx); err != nil {
		t.Fatalf("Wait gone: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for containsPid(g.Members(), gone.Pid()) {
		if time.Now().After(deadline) {
			t.Fatalf("exited process %d still listed in Members %v", gone.Pid(), g.Members())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func containsPid(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

func TestGroup_StartNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()
	if _, err := g.Start(context.Background(), Command("processkit-no-such-program-xyz")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestGroup_ConcurrentStartClose overlaps Start (which appends to the job's
// member set) with Close (which reads it) — under -race this catches an unguarded
// shared-state mutation in the platform job.
func TestGroup_ConcurrentStartClose(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = g.Start(context.Background(), Command(selfExe(t)).WithEnv(helperEnv("sleep")...))
		}()
	}
	time.Sleep(30 * time.Millisecond)
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	_ = g.Close() // idempotent
}

// TestGroup_StartAfterClose verifies a Start on an already-closed group is
// refused (errGroupClosed) rather than leaving an uncontained child.
func TestGroup_StartAfterClose(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = g.Start(context.Background(), Command(selfExe(t)).WithEnv(helperEnv("sleep")...))
	if !errors.Is(err, errGroupClosed) {
		t.Fatalf("Start after Close: err = %v, want errGroupClosed", err)
	}
}

// TestGroup_StartRacingCloseNoLeak overlaps streaming Starts (whose Lines channel
// is never drained, so each drain would block under the default OverflowBlock)
// with Close, then asserts every successfully-started process is reaped — i.e.
// Close reaches even a process registered as it was closing, leaking no
// drain/reap goroutine and orphaning no child. Run under -race.
func TestGroup_StartRacingCloseNoLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}

	var mu sync.Mutex
	var started []*RunningProcess
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := g.Start(context.Background(),
				Command(selfExe(t)).WithEnv(helperEnv("emitlines", "PK_LINES=500")...),
				StreamLines(), BufferLines(1))
			if err != nil {
				return // lost the race to Close — refused, no leak
			}
			mu.Lock()
			started = append(started, p)
			mu.Unlock()
		}()
	}
	time.Sleep(20 * time.Millisecond) // let some Starts land before we close
	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	_ = g.Close() // idempotent

	// Every started process must be reaped — Wait returns promptly, not hangs.
	for _, p := range started {
		wctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if _, err := p.Wait(wctx); err != nil {
			cancel()
			t.Fatalf("process %d not reaped after Close: %v — goroutine leak", p.Pid(), err)
		}
		cancel()
	}
}

// TestGroup_SignalKill tears the group down with SignalKill — the one portable
// signal, routed to the atomic whole-tree kill on every platform.
func TestGroup_SignalKill(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	p, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := p.Pid()
	if err := g.Signal(SignalKill); err != nil {
		t.Fatalf("Signal(SignalKill): %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive after SignalKill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestGroup_Adopt brings an externally-started process into the group and confirms
// Close reaps it — the no-orphan guarantee extended to an adopted process.
func TestGroup_Adopt(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess / kill-on-drop test")
	}
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}

	// Start a process OUTSIDE the group, directly via os/exec.
	ext := exec.Command(selfExe(t))
	ext.Env = helperEnv("sleep")
	if err := ext.Start(); err != nil {
		_ = g.Close()
		t.Fatalf("start external: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the child finish exec'ing before adopting

	// Reap the external process in the background, so a kill is observed promptly:
	// a killed-but-unreaped zombie still looks alive to a bare pid probe (notably on
	// macOS, which has no zombie-aware /proc). Wait completing IS the kill signal.
	waited := make(chan error, 1)
	go func() { waited <- ext.Wait() }()

	if err := g.Adopt(ext.Process); err != nil {
		_ = ext.Process.Kill()
		_ = g.Close()
		t.Fatalf("Adopt: %v", err)
	}
	if err := g.Close(); err != nil { // tears down the adopted process
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-waited: // Close killed and we reaped the adopted process — contained.
	case <-time.After(3 * time.Second):
		_ = ext.Process.Kill()
		t.Fatal("adopted process still running 3s after Close — Adopt did not contain it")
	}
}

func TestSignal_String(t *testing.T) {
	cases := map[Signal]string{
		SignalTerm:    "SIGTERM",
		SignalKill:    "SIGKILL",
		SignalUsr1:    "SIGUSR1",
		RawSignal(28): "signal 28",
	}
	for sig, want := range cases {
		if got := sig.String(); got != want {
			t.Errorf("Signal.String() = %q, want %q", got, want)
		}
	}
	if !SignalKill.isKill() || SignalTerm.isKill() {
		t.Fatal("isKill() should be true only for SignalKill")
	}
}

func waitPidFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %s was not written in time", path)
			return 0
		}
		time.Sleep(20 * time.Millisecond)
	}
}

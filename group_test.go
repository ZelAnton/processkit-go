package processkit

import (
	"context"
	"errors"
	"os"
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

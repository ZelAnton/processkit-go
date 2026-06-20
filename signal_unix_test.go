//go:build unix

package processkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReal_Signalled covers the third Outcome arm (Signalled), which is Unix-only.
// The "selfsig" helper SIGKILLs itself, so the run reports a signal kill.
func TestReal_Signalled(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	res, err := Command(selfExe(t)).WithEnv(helperEnv("selfsig")...).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if _, ok := res.Outcome().Signal(); !ok {
		t.Fatalf("expected a signalled outcome, got %v", res.Outcome())
	}
	if _, ok := res.Code(); ok {
		t.Fatal("a signalled outcome must have no exit code")
	}
}

// TestGroup_SignalTerm delivers SIGTERM to the group; the helper catches it and
// exits 0 gracefully (Unix only — Windows has no signal tier).
func TestGroup_SignalTerm(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	p, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("termexit")...))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the child install its SIGTERM handler

	if err := g.Signal(SignalTerm); err != nil {
		t.Fatalf("Signal(SignalTerm): %v", err)
	}
	out, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if c, ok := out.Code(); !ok || c != 0 {
		t.Fatalf("outcome = %v, want exited(0) (graceful SIGTERM)", out)
	}
}

// TestGroup_SuspendResume freezes a ticking process and confirms it stops making
// progress, then resumes it and confirms it continues (Unix only).
func TestGroup_SuspendResume(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	tick := filepath.Join(t.TempDir(), "tick")
	if _, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("tickfile", "PK_TICKFILE="+tick, "PK_DELAY_MS=5")...)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	size := func() int64 {
		fi, _ := os.Stat(tick)
		if fi == nil {
			return 0
		}
		return fi.Size()
	}
	// Wait until it's ticking.
	deadline := time.Now().Add(2 * time.Second)
	for size() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("process never started ticking")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := g.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let any in-flight tick settle
	frozen := size()
	time.Sleep(300 * time.Millisecond)
	if grew := size(); grew != frozen {
		t.Fatalf("a suspended process kept ticking: %d → %d", frozen, grew)
	}

	if err := g.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for size() <= frozen {
		if time.Now().After(deadline) {
			t.Fatalf("a resumed process did not tick again (still %d)", frozen)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

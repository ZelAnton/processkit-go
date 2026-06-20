//go:build unix

package processkit

import (
	"context"
	"testing"
	"time"
)

// TestGroup_ShutdownGraceful verifies the Unix graceful tier: SIGTERM gives a
// well-behaved child a chance to exit cleanly before the hard kill. The "termexit"
// helper exits 0 on SIGTERM, so a graceful Shutdown must leave it Exited(0), not
// SIGKILLed.
func TestGroup_ShutdownGraceful(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	p, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("termexit")...))
	if err != nil {
		_ = g.Close()
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let the child install its SIGTERM handler

	if err := g.Shutdown(ctx, ShutdownGrace(2*time.Second)); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	outcome, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if c, ok := outcome.Code(); !ok || c != 0 {
		t.Fatalf("graceful shutdown: want exited(0), got %v", outcome)
	}
}

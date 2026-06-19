//go:build unix

package processkit

import (
	"context"
	"testing"
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

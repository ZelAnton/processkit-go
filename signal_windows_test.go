//go:build windows

package processkit

import (
	"context"
	"errors"
	"testing"
)

// TestGroup_SignalUnsupported confirms the honesty rule on Windows: a Job Object
// has no signal tier, so any non-kill signal is an explicit ErrUnsupported, never a
// silent no-op. (SignalKill works — it routes to the atomic job kill.)
func TestGroup_SignalUnsupported(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()
	if _, err := g.Start(context.Background(), Command(selfExe(t)).WithEnv(helperEnv("sleep")...)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for _, sig := range []Signal{SignalTerm, SignalInt, SignalHup, RawSignal(28)} {
		if err := g.Signal(sig); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Signal(%s) = %v, want ErrUnsupported on Windows", sig, err)
		}
	}
}

func TestGroup_SuspendResumeUnsupported(t *testing.T) {
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()
	if err := g.Suspend(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Suspend() = %v, want ErrUnsupported on Windows", err)
	}
	if err := g.Resume(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Resume() = %v, want ErrUnsupported on Windows", err)
	}
}

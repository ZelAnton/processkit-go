package processkit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReal_ExitAndCapture(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	exe := selfExe(t)
	env := helperEnv("exit", "PK_CODE=2", "PK_STDOUT=hello", "PK_STDERR=oops")

	res, err := Command(exe).WithEnv(env...).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if c, ok := res.Code(); !ok || c != 2 {
		t.Fatalf("Code() = (%d, %v), want (2, true)", c, ok)
	}
	if res.Stdout() != "hello" {
		t.Fatalf("Stdout = %q, want %q", res.Stdout(), "hello")
	}
	if res.Stderr() != "oops" {
		t.Fatalf("Stderr = %q, want %q", res.Stderr(), "oops")
	}
	if res.Mechanism() == MechanismUnknown {
		t.Fatal("Mechanism should be set for a real run")
	}

	_, err = Command(exe).WithEnv(env...).Run(context.Background())
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Run on a non-zero exit: want *ExitError, got %v", err)
	}
}

func TestReal_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	out, err := Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_STDOUT=ok")...).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Run = %q, want %q", out, "ok")
	}
}

func TestReal_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	_, err := Command("processkit-definitely-no-such-program-xyz").Output(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReal_TimeoutCaptured(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	res, err := Command(selfExe(t)).
		WithEnv(helperEnv("sleep")...).
		WithTimeout(300 * time.Millisecond).
		Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !res.TimedOut() {
		t.Fatalf("expected a captured timeout, got %v", res.Outcome())
	}
}

func TestReal_Cancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := Command(selfExe(t)).WithEnv(helperEnv("sleep")...).Output(ctx)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CancelError must unwrap to context.Canceled, got %v", err)
	}
}

func TestReal_PreCancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the run starts — must short-circuit without spawning
	_, err := Command(selfExe(t)).WithEnv(helperEnv("exit")...).Output(ctx)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("want ErrCancelled for a pre-cancelled context, got %v", err)
	}
}

// TestReal_ParentDeadlineIsCancel pins the semantic: the caller's *own* context
// deadline (as opposed to Cmd.WithTimeout) is an error (ErrCancelled), and it
// unwraps to context.DeadlineExceeded — it is NOT a captured timeout.
func TestReal_ParentDeadlineIsCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Command(selfExe(t)).WithEnv(helperEnv("sleep")...).Output(ctx)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("parent ctx deadline: want ErrCancelled, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent ctx deadline must unwrap to context.DeadlineExceeded, got %v", err)
	}
}

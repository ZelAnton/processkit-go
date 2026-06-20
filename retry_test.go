package processkit

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func always(error) bool { return true }
func never(error) bool  { return false }

// retryCmd builds a command over a scripted runner with the given retry policy and
// no backoff, for fast deterministic retry-logic tests.
func retryCmd(sr *scriptedRunner, maxAttempts int, retryIf func(error) bool) *Cmd {
	return Command("svc").WithRunner(sr).WithRetry(maxAttempts, 0, retryIf)
}

func TestRetry_SucceedsAfterFailures(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{exitReply(1), exitReply(1), exitReply(0)}}
	out, err := retryCmd(sr, 3, always).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "" {
		t.Fatalf("out = %q, want empty", out)
	}
	if sr.calls != 3 {
		t.Fatalf("calls = %d, want 3", sr.calls)
	}
}

func TestRetry_StopsOnClassifierFalse(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{exitReply(1), exitReply(0)}}
	_, err := retryCmd(sr, 3, never).Run(context.Background())
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if sr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (classifier rejected a retry)", sr.calls)
	}
}

func TestRetry_CapsAtMaxAttempts(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{exitReply(1), exitReply(1), exitReply(1), exitReply(1)}}
	_, err := retryCmd(sr, 3, always).Run(context.Background())
	if !errors.As(err, new(*ExitError)) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if sr.calls != 3 {
		t.Fatalf("calls = %d, want 3 (the attempt budget)", sr.calls)
	}
}

func TestRetry_NoPolicyRunsOnce(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{exitReply(1), exitReply(0)}}
	_, err := Command("svc").WithRunner(sr).Run(context.Background())
	if !errors.As(err, new(*ExitError)) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if sr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (no retry policy)", sr.calls)
	}
}

func TestRetry_MaxAttemptsOneRunsOnce(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{exitReply(1), exitReply(0)}}
	_, err := retryCmd(sr, 1, always).Run(context.Background())
	if !errors.As(err, new(*ExitError)) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if sr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (maxAttempts 1)", sr.calls)
	}
}

func TestRetry_NilClassifierRunsOnce(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{exitReply(1), exitReply(0)}}
	_, err := Command("svc").WithRunner(sr).WithRetry(3, 0, nil).Run(context.Background())
	if !errors.As(err, new(*ExitError)) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if sr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (a nil classifier retries nothing)", sr.calls)
	}
}

func TestRetry_FirstSuccessNoRetry(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{exitReply(0)}}
	if _, err := retryCmd(sr, 3, always).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (succeeded first try)", sr.calls)
	}
}

func TestRetry_OkCodeIsSuccessNotRetried(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{exitReply(2, 2)}} // exit 2, accepted
	if _, err := retryCmd(sr, 3, always).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (an ok-code exit is success)", sr.calls)
	}
}

func TestRetry_CancelIsTerminal(t *testing.T) {
	ce := &CancelError{Program: "svc", Cause: context.Canceled}
	sr := &scriptedRunner{replies: []scriptReply{errReply(ce), exitReply(0)}}
	_, err := retryCmd(sr, 3, always).Run(context.Background())
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled (terminal, never retried)", err)
	}
	if sr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (cancel is terminal)", sr.calls)
	}
}

func TestRetry_SpawnErrorIsClassified(t *testing.T) {
	sr := &scriptedRunner{replies: []scriptReply{errReply(&NotFoundError{Program: "svc"}), exitReply(0)}}
	if _, err := retryCmd(sr, 3, always).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sr.calls != 2 {
		t.Fatalf("calls = %d, want 2 (a spawn error retried, then success)", sr.calls)
	}
}

func TestRetry_CancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sr := &scriptedRunner{replies: []scriptReply{exitReply(1), exitReply(0)}}
	cmd := Command("svc").WithRunner(sr).WithRetry(3, 500*time.Millisecond, always)
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()
	_, err := cmd.Run(ctx)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled (cancel during backoff)", err)
	}
	if sr.calls != 1 {
		t.Fatalf("calls = %d, want 1 (cancelled before the retry)", sr.calls)
	}
}

func TestRetry_ExitCodeRetriesTimeoutButNotNonZeroExit(t *testing.T) {
	// ExitCode treats a non-zero exit as success (it returns the code); only a
	// no-code outcome (timeout/signal) is an error, so only that is retried.
	sr := &scriptedRunner{replies: []scriptReply{timeoutReply(), exitReply(5)}}
	code, err := Command("svc").WithRunner(sr).
		WithRetry(3, 0, func(err error) bool { return errors.Is(err, ErrTimeout) }).
		ExitCode(context.Background())
	if err != nil {
		t.Fatalf("ExitCode: %v", err)
	}
	if code != 5 {
		t.Fatalf("code = %d, want 5", code)
	}
	if sr.calls != 2 {
		t.Fatalf("calls = %d, want 2 (timeout retried, then a non-zero exit is success)", sr.calls)
	}
}

// --- real-subprocess tests ---

func TestRetry_RealFlakySucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	counter := filepath.Join(t.TempDir(), "count")
	out, err := Command(selfExe(t)).
		WithEnv(helperEnv("flakyfile", "PK_COUNTER="+counter, "PK_SUCCEED_AT=3")...).
		WithRetry(5, time.Millisecond, always).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (a 5-attempt retry should reach success at attempt 3)", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q, want \"ok\"", out)
	}
}

func TestRetry_RealExhausts(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	_, err := Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=1")...).
		WithRetry(3, time.Millisecond, always).
		Run(context.Background())
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if c, ok := ee.Outcome.Code(); !ok || c != 1 {
		t.Fatalf("exhausted code = %v, want 1", ee.Outcome)
	}
}

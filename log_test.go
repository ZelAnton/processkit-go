package processkit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a concurrency-safe sink: the reap goroutine logs "process exited"
// while the main goroutine logs "child spawned", so the slog writer must be locked.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newCapture returns a JSON logger writing into a sink we can scan.
func newCapture() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// A nil logger (the default) must be a complete no-op — no output, no panic.
func TestLog_NoOpDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	// No WithLogger anywhere — must not panic and must produce nothing observable.
	if _, err := Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=0")...).Output(ctx); err != nil {
		t.Fatalf("Output: %v", err)
	}
	g, err := NewGroup() // no WithLogger
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	g.Close() // exercises the terminating path with a nil logger — must not panic
}

// The capture must show the per-run lifecycle for a Cmd.WithLogger run.
func TestLog_CmdLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	logger, buf := newCapture()
	_, err := Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=0")...).WithLogger(logger).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"child spawned", "process exited", "pid", "outcome", "elapsed_ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, out)
		}
	}
}

// THE security invariant: argv and env values must NEVER reach a log record.
func TestLog_NeverLogsArgvOrEnv(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	const secretArg = "SECRET_ARG_do_not_log_42"
	const secretEnv = "SECRET_ENV_do_not_log_99"

	logger, buf := newCapture()
	// The helper ignores the extra arg and the secret env var; we only care that the
	// logger never emits them.
	cmd := Command(selfExe(t), secretArg).
		WithEnv(helperEnv("exit", "PK_CODE=0", "PK_SECRET="+secretEnv)...).
		WithLogger(logger)
	if _, err := cmd.Output(context.Background()); err != nil {
		t.Fatalf("Output: %v", err)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("expected lifecycle log output")
	}
	if strings.Contains(out, secretArg) {
		t.Errorf("argv leaked into the log:\n%s", out)
	}
	if strings.Contains(out, secretEnv) {
		t.Errorf("env value leaked into the log:\n%s", out)
	}
}

// fakeLogRunner returns a fixed Result/err — the hermetic seam for retry/supervisor
// logging, no subprocess.
type fakeLogRunner struct {
	res *Result
	err error
}

func (f fakeLogRunner) Output(context.Context, Invocation) (*Result, error) { return f.res, f.err }

// A retried run logs the retry event with the upcoming attempt number.
func TestLog_Retry(t *testing.T) {
	logger, buf := newCapture()
	runner := fakeLogRunner{res: &Result{program: "x", outcome: exited(1)}}
	_, err := Command("x").
		WithRunner(runner).
		WithLogger(logger).
		WithRetry(2, 0, func(error) bool { return true }).
		Run(context.Background())
	if err == nil {
		t.Fatal("expected the run to fail after exhausting retries")
	}
	out := buf.String()
	if !strings.Contains(out, "retrying after a retryable failure") {
		t.Errorf("missing retry event:\n%s", out)
	}
	if !strings.Contains(out, `"attempt":2`) {
		t.Errorf("retry event should report the upcoming attempt 2:\n%s", out)
	}
}

// A supervised crash loop logs each restart.
func TestLog_SupervisorRestart(t *testing.T) {
	logger, buf := newCapture()
	runner := fakeLogRunner{res: &Result{program: "svc", outcome: exited(1)}} // always crashes
	_, err := Supervise(Command("svc")).
		WithRunner(runner).
		WithMaxRestarts(1).
		WithBackoff(0, 1).
		WithJitter(false).
		WithLogger(logger).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "supervisor restarting child") {
		t.Errorf("missing restart event:\n%s", out)
	}
	if !strings.Contains(out, `"restart":1`) {
		t.Errorf("restart event should report restart 1:\n%s", out)
	}
}

// A logger on the supervised Cmd must still produce its per-run spawn/exit events
// under supervision (the supervisor threads it into the default runner).
func TestLog_SupervisorThreadsCmdLogger(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	logger, buf := newCapture()
	// A clean run ends supervision after one incarnation (RestartOnCrash).
	_, err := Supervise(Command(selfExe(t)).WithEnv(helperEnv("exit", "PK_CODE=0")...).WithLogger(logger)).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"child spawned", "process exited"} {
		if !strings.Contains(out, want) {
			t.Errorf("supervised run missing %q:\n%s", want, out)
		}
	}
}

// Group teardown logs the terminate event (no subprocess needed).
func TestLog_GroupTerminate(t *testing.T) {
	logger, buf := newCapture()
	g, err := NewGroup(WithLogger(logger))
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	g.Close()
	if !strings.Contains(buf.String(), "terminating every process in the group") {
		t.Errorf("missing terminate event:\n%s", buf.String())
	}
}

// The event levels follow the convention: lifecycle at DEBUG, anomalies at WARN.
func TestLog_TimeoutIsWarn(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	logger, buf := newCapture()
	// A 50ms timeout on a 10s sleeper: the run times out and logs a WARN.
	_, _ = Command(selfExe(t)).WithEnv(helperEnv("sleep")...).
		WithTimeout(50 * time.Millisecond).WithLogger(logger).Output(context.Background())
	out := buf.String()
	if !strings.Contains(out, "timeout elapsed; killing the tree") {
		t.Fatalf("missing timeout event:\n%s", out)
	}
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("timeout should be logged at WARN:\n%s", out)
	}
}

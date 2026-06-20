package processkit

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stage builds a pipeline stage that re-execs the test binary in the given mode.
func stage(t *testing.T, mode string, extra ...string) *Cmd {
	t.Helper()
	return Command(selfExe(t)).WithEnv(helperEnv(mode, extra...)...)
}

func TestPipeline_RelayAndRun(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	// emitlines writes "out 1".."out 3"; upper upper-cases the relayed stream.
	out, err := Pipe(
		stage(t, "emitlines", "PK_LINES=3"),
		stage(t, "upper"),
	).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "OUT 1\nOUT 2\nOUT 3"; out != want {
		t.Fatalf("pipeline output = %q, want %q", out, want)
	}
}

func TestPipeline_FirstStageStdin(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	out, err := Pipe(
		stage(t, "catlines"),
		stage(t, "upper"),
	).WithStdin(strings.NewReader("hi\nbye\n")).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "ECHO: HI\nECHO: BYE"; out != want {
		t.Fatalf("pipeline output = %q, want %q", out, want)
	}
}

func TestPipeline_PipefailAttributesFirstFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	// Middle stage exits 3; last stage is clean. pipefail blames the middle stage.
	_, err := Pipe(
		stage(t, "emitlines", "PK_LINES=2"),
		stage(t, "drainexit", "PK_CODE=3", "PK_STDERR=middle boom"),
		stage(t, "upper"),
	).Run(context.Background())
	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("err = %v, want *ExitError", err)
	}
	if c, ok := exit.Outcome.Code(); !ok || c != 3 {
		t.Fatalf("attributed code = %v, want 3", exit.Outcome)
	}
}

// TestPipeline_HeadPatternWithUnchecked is the canonical `producer | head` case:
// the producer is killed (SIGPIPE on Unix) when head stops reading; marking it
// unchecked keeps the chain a success with head's output.
func TestPipeline_HeadPatternWithUnchecked(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	out, err := Pipe(
		stage(t, "emitlines", "PK_LINES=100000").WithUncheckedInPipe(),
		stage(t, "headone"),
	).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (the unchecked producer should not fail the chain)", err)
	}
	if out != "out 1" {
		t.Fatalf("head output = %q, want %q", out, "out 1")
	}
}

func TestPipeline_WholeChainTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	res, err := Pipe(
		stage(t, "sleep"), // sleeps 10s, never produces
		stage(t, "upper"),
	).WithTimeout(150 * time.Millisecond).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !res.TimedOut() {
		t.Fatalf("outcome = %v, want timedOut", res.Outcome())
	}
	if res.Stdout() != "" {
		t.Fatalf("stdout = %q, want empty (a whole-chain timeout keeps no partial output)", res.Stdout())
	}
	if !strings.Contains(res.Program(), " | ") {
		t.Fatalf("program = %q, want the joined chain name", res.Program())
	}
}

func TestPipeline_PerStageTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	// The first stage has its own short deadline; the chain has none.
	res, err := Pipe(
		stage(t, "sleep").WithTimeout(150*time.Millisecond),
		stage(t, "upper"),
	).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !res.TimedOut() {
		t.Fatalf("outcome = %v, want the first stage's timeout attributed", res.Outcome())
	}
}

func TestPipeline_Cancel(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(80 * time.Millisecond); cancel() }()
	_, err := Pipe(
		stage(t, "sleep"),
		stage(t, "upper"),
	).Output(ctx)
	var ce *CancelError
	if !errors.As(err, &ce) || !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want *CancelError / ErrCancelled", err)
	}
}

// TestPipeline_KillOnDropReapsGrandchild confirms the whole chain shares one
// kill-on-drop container: a grandchild spawned by a stage is reaped by the time
// Output returns.
func TestPipeline_KillOnDropReapsGrandchild(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess / kill-on-drop test")
	}
	// `tree` spawns a lingering grandchild and prints "grandchild=<pid>"; upper
	// relays it upper-cased so we can read the pid from the captured output.
	res, err := Pipe(
		stage(t, "tree"),
		stage(t, "upper"),
	).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	pid := parseGrandchildPid(t, res.Stdout())
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive after the pipeline finished — orphan leak", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPipeline_FirstStageNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	_, err := Pipe(
		Command("processkit-no-such-program-xyz"),
		stage(t, "upper"),
	).Output(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPipeline_TooFewStages(t *testing.T) {
	_, err := Pipe(Command("only-one")).Output(context.Background())
	if !errors.Is(err, ErrTooFewStages) {
		t.Fatalf("err = %v, want ErrTooFewStages", err)
	}
}

// parseGrandchildPid extracts the pid from the upper-cased "GRANDCHILD=<pid>" line.
func parseGrandchildPid(t *testing.T, out string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "GRANDCHILD="); ok {
			pid, err := strconv.Atoi(rest)
			if err != nil {
				t.Fatalf("bad grandchild pid %q: %v", rest, err)
			}
			return pid
		}
	}
	t.Fatalf("no GRANDCHILD= line in pipeline output %q", out)
	return 0
}

package processkit

import (
	"context"
	"errors"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeLive builds a RunningProcess with a fed line channel and a live (open) done
// channel — the hermetic seam for the line probe, no subprocess.
func fakeLive(lines chan Line) *RunningProcess {
	return &RunningProcess{program: "svc", lines: lines, done: make(chan struct{})}
}

// fakeExited builds a RunningProcess that has already been reaped.
func fakeExited() *RunningProcess {
	done := make(chan struct{})
	close(done)
	return &RunningProcess{program: "svc", done: done}
}

func TestWaitForLine_Match(t *testing.T) {
	lines := make(chan Line, 4)
	lines <- Line{Stream: StreamStdout, Text: "starting up"}
	lines <- Line{Stream: StreamStderr, Text: "server ready on :8080"}
	got, err := fakeLive(lines).WaitForLine(context.Background(),
		func(s string) bool { return strings.Contains(s, "ready") }, time.Second)
	if err != nil {
		t.Fatalf("WaitForLine: %v", err)
	}
	if got != "server ready on :8080" {
		t.Fatalf("matched line = %q", got)
	}
}

func TestWaitForLine_MatchInBufferedAtEntryZeroDeadline(t *testing.T) {
	lines := make(chan Line, 4)
	lines <- Line{Stream: StreamStdout, Text: "warming up"}
	lines <- Line{Stream: StreamStdout, Text: "ready!"}
	got, err := fakeLive(lines).WaitForLine(context.Background(),
		func(s string) bool { return s == "ready!" }, 0) // a zero deadline still scans the buffer
	if err != nil {
		t.Fatalf("zero-deadline buffered match: %v", err)
	}
	if got != "ready!" {
		t.Fatalf("matched line = %q", got)
	}
}

// TestWaitForLine_FloodRespectsDeadline guards against a flood of non-matching
// lines starving the deadline — the probe must still give up at its within.
func TestWaitForLine_FloodRespectsDeadline(t *testing.T) {
	lines := make(chan Line)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case lines <- Line{Stream: StreamStdout, Text: "noise"}:
			case <-stop:
				return
			}
		}
	}()
	start := time.Now()
	_, err := fakeLive(lines).WaitForLine(context.Background(),
		func(s string) bool { return s == "never" }, 100*time.Millisecond)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the flood starved the deadline: took %v", elapsed)
	}
}

func TestWaitForLine_DeadlineNotReady(t *testing.T) {
	_, err := fakeLive(make(chan Line)).WaitForLine(context.Background(),
		func(string) bool { return true }, 80*time.Millisecond)
	var nr *NotReadyError
	if !errors.As(err, &nr) || nr.Probe != "line" {
		t.Fatalf("err = %v, want a line *NotReadyError", err)
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want errors.Is ErrNotReady", err)
	}
}

func TestWaitForLine_StreamEndIsNotReadyFast(t *testing.T) {
	lines := make(chan Line)
	close(lines) // the process produced all its output / exited
	start := time.Now()
	_, err := fakeLive(lines).WaitForLine(context.Background(),
		func(string) bool { return true }, 10*time.Second)
	if time.Since(start) > 2*time.Second {
		t.Fatal("a closed stream should fail fast, not wait the full deadline")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
}

func TestWaitForLine_RequiresStreaming(t *testing.T) {
	p := &RunningProcess{program: "svc", done: make(chan struct{})} // lines == nil
	_, err := p.WaitForLine(context.Background(), func(string) bool { return true }, time.Second)
	if !errors.Is(err, errStreamNotEnabled) {
		t.Fatalf("err = %v, want errStreamNotEnabled", err)
	}
}

func TestWaitForLine_CtxCancelIsBareError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := fakeLive(make(chan Line)).WaitForLine(ctx, func(string) bool { return true }, 10*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the bare context.Canceled", err)
	}
	if errors.Is(err, ErrNotReady) {
		t.Fatalf("a cancelled probe should be ctx.Err(), not ErrNotReady: %v", err)
	}
}

func TestWaitForPort_Open(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	if err := fakeExitedNot().WaitForPort(context.Background(), ln.Addr().String(), 2*time.Second); err != nil {
		t.Fatalf("WaitForPort on an open port: %v", err)
	}
}

func TestWaitForPort_ClosedIsNotReady(t *testing.T) {
	addr := freeAddr(t)
	err := fakeExitedNot().WaitForPort(context.Background(), addr, 150*time.Millisecond)
	var nr *NotReadyError
	if !errors.As(err, &nr) || nr.Probe != "port" {
		t.Fatalf("err = %v, want a port *NotReadyError", err)
	}
	if nr.Cause == nil {
		t.Fatalf("port NotReadyError should carry the last dial error")
	}
}

func TestWaitForPort_ProcessExitedIsFast(t *testing.T) {
	addr := freeAddr(t)
	start := time.Now()
	err := fakeExited().WaitForPort(context.Background(), addr, 10*time.Second)
	if time.Since(start) > 2*time.Second {
		t.Fatal("an exited process should fail the port probe fast")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
}

func TestWaitFor_TrueAfterAFewTries(t *testing.T) {
	n := 0
	err := fakeExitedNot().WaitFor(context.Background(), func(context.Context) bool {
		n++
		return n >= 3
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if n < 3 {
		t.Fatalf("check called %d times, want >= 3", n)
	}
}

func TestWaitFor_NeverIsNotReady(t *testing.T) {
	err := fakeExitedNot().WaitFor(context.Background(), func(context.Context) bool { return false }, 120*time.Millisecond)
	var nr *NotReadyError
	if !errors.As(err, &nr) || nr.Probe != "predicate" {
		t.Fatalf("err = %v, want a predicate *NotReadyError", err)
	}
}

// TestProbe_HugeWithinDoesNotOverflow guards against a near-max within wrapping
// time arithmetic into an immediate not-ready: the probe must keep waiting (here
// until the context deadline), not return at once.
func TestProbe_HugeWithinDoesNotOverflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := fakeExitedNot().WaitFor(ctx, func(context.Context) bool { return false }, time.Duration(math.MaxInt64))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded (a huge within must not overflow to NotReady)", err)
	}
}

func TestWaitFor_ProcessExitedIsFast(t *testing.T) {
	start := time.Now()
	err := fakeExited().WaitFor(context.Background(), func(context.Context) bool { return false }, 10*time.Second)
	if time.Since(start) > 2*time.Second {
		t.Fatal("an exited process should fail the predicate probe fast")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
}

// fakeExitedNot is a live (not-exited) process with no line channel — fine for the
// port/predicate probes, which don't read lines.
func fakeExitedNot() *RunningProcess {
	return &RunningProcess{program: "svc", done: make(chan struct{})}
}

// freeAddr returns a 127.0.0.1 address that is (briefly) not listening, by binding
// then immediately releasing an ephemeral port.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// --- real-subprocess tests ---

func TestWaitForLine_Real(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	p, err := g.Start(ctx,
		Command(selfExe(t)).WithEnv(helperEnv("readyline", "PK_READY=listening now")...),
		StreamLines())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	line, err := p.WaitForLine(ctx, func(s string) bool { return strings.Contains(s, "listening") }, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForLine: %v", err)
	}
	if !strings.Contains(line, "listening") {
		t.Fatalf("matched line = %q", line)
	}
	if !containsPid(memberPids(g), p.Pid()) {
		t.Fatal("the probe killed the child — a probe must leave it running")
	}
}

func TestWaitForPort_Real(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	p, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("listen")...), StreamLines())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The child announces its ephemeral port; read it with the line probe, then
	// confirm the port with the port probe.
	line, err := p.WaitForLine(ctx, func(s string) bool { return strings.HasPrefix(s, "PORT=") }, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForLine(PORT): %v", err)
	}
	addr := strings.TrimPrefix(line, "PORT=")
	if err := p.WaitForPort(ctx, addr, 5*time.Second); err != nil {
		t.Fatalf("WaitForPort(%s): %v", addr, err)
	}
	if !containsPid(memberPids(g), p.Pid()) {
		t.Fatal("the probe killed the child")
	}
}

func TestWaitForLine_RealNotReadyLeavesChildAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	p, err := g.Start(ctx, Command(selfExe(t)).WithEnv(helperEnv("sleep")...), StreamLines())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = p.WaitForLine(ctx, func(string) bool { return true }, 250*time.Millisecond)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady (the silent child never matched)", err)
	}
	if !containsPid(memberPids(g), p.Pid()) {
		t.Fatal("a NotReady probe must leave the child running")
	}
}

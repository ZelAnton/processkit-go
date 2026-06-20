package processkit

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStream_LinesChannel streams a child line-by-line over the merged channel
// and checks both stdout and stderr lines arrive, tagged, and the channel closes.
func TestStream_LinesChannel(t *testing.T) {
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
		Command(selfExe(t)).WithEnv(helperEnv("emitlines", "PK_LINES=4", "PK_STDERR_EVERY=2")...),
		StreamLines())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var outs, errs []string
	for ln := range p.Lines() { // ranges to completion: channel closes at EOF
		switch ln.Stream {
		case StreamStdout:
			outs = append(outs, ln.Text)
		case StreamStderr:
			errs = append(errs, ln.Text)
		}
	}
	if want := []string{"out 1", "out 2", "out 3", "out 4"}; !equalStrings(outs, want) {
		t.Fatalf("stdout lines = %q, want %q", outs, want)
	}
	if want := []string{"err 2", "err 4"}; !equalStrings(errs, want) {
		t.Fatalf("stderr lines = %q, want %q", errs, want)
	}

	out, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if c, ok := out.Code(); !ok || c != 0 {
		t.Fatalf("outcome = %v, want exited(0)", out)
	}
}

// TestStream_Callbacks checks the OnStdoutLine / OnStderrLine handlers fire per
// line, in order, alongside a raw tee.
func TestStream_Callbacks(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	var mu sync.Mutex
	var outs, errs []string
	var tee bytes.Buffer
	p, err := g.Start(ctx,
		Command(selfExe(t)).WithEnv(helperEnv("emitlines", "PK_LINES=3", "PK_STDERR_EVERY=3")...),
		OnStdoutLine(func(s string) { mu.Lock(); outs = append(outs, s); mu.Unlock() }),
		OnStderrLine(func(s string) { mu.Lock(); errs = append(errs, s); mu.Unlock() }),
		WithStdout(&tee))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := p.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"out 1", "out 2", "out 3"}; !equalStrings(outs, want) {
		t.Fatalf("stdout callbacks = %q, want %q", outs, want)
	}
	if want := []string{"err 3"}; !equalStrings(errs, want) {
		t.Fatalf("stderr callbacks = %q, want %q", errs, want)
	}
	// The tee mirrors raw stdout bytes verbatim.
	if got := strings.Count(tee.String(), "out "); got != 3 {
		t.Fatalf("tee saw %d stdout lines, want 3 (%q)", got, tee.String())
	}
}

// TestStream_InteractiveStdin feeds lines to a child over time through an io.Pipe
// and reads its echoes back from the merged channel.
func TestStream_InteractiveStdin(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	pr, pw := io.Pipe()
	p, err := g.Start(ctx,
		Command(selfExe(t)).WithEnv(helperEnv("catlines")...),
		WithStdin(pr), StreamLines())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	go func() {
		for _, s := range []string{"ping", "pong"} {
			_, _ = io.WriteString(pw, s+"\n")
		}
		_ = pw.Close() // EOF → the child stops reading and exits
	}()

	var got []string
	for ln := range p.Lines() {
		got = append(got, ln.Text)
	}
	if want := []string{"echo: ping", "echo: pong"}; !equalStrings(got, want) {
		t.Fatalf("echoed lines = %q, want %q", got, want)
	}
}

// TestStream_CancelMidStreamReapsAndCloses is the Stage 3 gate: cancelling the
// start context mid-stream tears the process down and closes the line channel.
func TestStream_CancelMidStreamReapsAndCloses(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess / kill-on-drop test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // safety net if the stream ends before we cancel below
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	// A long stream: 1000 lines, 5ms apart (~5s) — plenty of headroom to cancel mid-flight.
	p, err := g.Start(ctx,
		Command(selfExe(t)).WithEnv(helperEnv("emitlines", "PK_LINES=1000", "PK_DELAY_MS=5")...),
		StreamLines())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := 0
	for ln := range p.Lines() {
		got++
		if got == 3 {
			cancel() // tear down mid-stream
		}
		_ = ln
	}
	// The range above only returns once the channel is closed — which proves the
	// cancel reaped the tree and closed the channel. A few buffered lines may
	// arrive after cancel; we only require that it terminated.
	if got < 3 {
		t.Fatalf("received %d lines before close, expected the stream to have started", got)
	}

	// The process is gone; Wait returns promptly with a terminal outcome.
	wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wcancel()
	if _, err := p.Wait(wctx); err != nil {
		t.Fatalf("Wait after cancel: %v (process was not reaped)", err)
	}
}

// TestStream_DropNewestNeverStalls confirms OverflowDropNewest lets a child run
// to completion even when the consumer never reads, counting the drops.
func TestStream_DropNewestNeverStalls(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}
	defer g.Close()

	// Many lines, a tiny buffer, and we deliberately never read the channel.
	p, err := g.Start(ctx,
		Command(selfExe(t)).WithEnv(helperEnv("emitlines", "PK_LINES=200")...),
		StreamLines(), BufferLines(1), OnOverflow(OverflowDropNewest))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v (drop-newest should never stall the child)", err)
	}
	if c, ok := out.Code(); !ok || c != 0 {
		t.Fatalf("outcome = %v, want exited(0)", out)
	}
	if p.DroppedLines() == 0 {
		t.Fatalf("expected some dropped lines with an unread 1-line buffer and 200 lines")
	}
}

// TestStream_CloseReleasesAbandonedDrain guards against a goroutine leak: a
// process is started with streaming under the default OverflowBlock, its Lines()
// channel is never read, then the group is closed. Close must release the
// backpressured drain so reap completes and Wait returns promptly.
func TestStream_CloseReleasesAbandonedDrain(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	ctx := context.Background()
	g, err := NewGroup()
	if err != nil {
		t.Fatalf("NewGroup: %v", err)
	}

	// Many lines, a tiny buffer, default OverflowBlock — the drain will block once
	// the buffer fills because we never read the channel.
	p, err := g.Start(ctx,
		Command(selfExe(t)).WithEnv(helperEnv("emitlines", "PK_LINES=500")...),
		StreamLines(), BufferLines(1))
	if err != nil {
		_ = g.Close()
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the buffer fill and the drain block

	if err := g.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wcancel()
	if _, err := p.Wait(wctx); err != nil {
		t.Fatalf("Wait after Close: %v — drain/reap goroutines leaked", err)
	}
}

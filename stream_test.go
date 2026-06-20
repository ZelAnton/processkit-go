package processkit

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// collect returns a callback that appends each line to *dst.
func collect(dst *[]string) func(string) {
	return func(s string) { *dst = append(*dst, s) }
}

func TestDrainLines_Splitting(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"lf", "a\nb\nc\n", []string{"a", "b", "c"}},
		{"crlf", "a\r\nb\r\n", []string{"a", "b"}},
		{"no-trailing-newline", "a\nb", []string{"a", "b"}},
		{"blank-lines", "\n\n", []string{"", ""}},
		{"empty", "", nil},
		{"only-remainder", "solo", []string{"solo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			drainLines(strings.NewReader(tc.in), lineSink{onLine: collect(&got)}, 0)
			if !equalStrings(got, tc.want) {
				t.Fatalf("drainLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDrainLines_LongLineChunked feeds a long, newline-free blob and asserts it
// is emitted as bounded fragments (no unbounded buffering, no aborted stream)
// whose concatenation is the original input.
func TestDrainLines_LongLineChunked(t *testing.T) {
	const max = 16 // bufio's minimum reader size — fragments cap at this
	blob := strings.Repeat("a", 40)
	var got []string
	drainLines(strings.NewReader(blob), lineSink{onLine: collect(&got)}, max)
	if len(got) < 2 {
		t.Fatalf("expected the blob to be chunked, got %d fragment(s)", len(got))
	}
	for i, frag := range got {
		if len(frag) > max {
			t.Fatalf("fragment %d is %d bytes, want <= %d", i, len(frag), max)
		}
	}
	if joined := strings.Join(got, ""); joined != blob {
		t.Fatalf("fragments rejoined = %q, want the original blob", joined)
	}
}

// TestDrainOne_TeeIsRawDecodedIsLines proves the tee receives verbatim bytes
// while the line consumers receive the decoded text.
func TestDrainOne_TeeIsRawDecodedIsLines(t *testing.T) {
	var tee bytes.Buffer
	var got []string
	cfg := &startConfig{
		decoder: func(r io.Reader) io.Reader { return &upperReader{r} },
	}
	cfg.drainOne(context.Background(), &RunningProcess{}, StreamStdout,
		strings.NewReader("ab\ncd\n"), &tee, collect(&got))

	if tee.String() != "ab\ncd\n" {
		t.Fatalf("tee = %q, want raw %q", tee.String(), "ab\ncd\n")
	}
	if want := []string{"AB", "CD"}; !equalStrings(got, want) {
		t.Fatalf("decoded lines = %q, want %q", got, want)
	}
}

// TestDrainOne_TeeOnlyNoLineSplit confirms a stream with only a tee (no line
// consumer) is copied through verbatim and never split.
func TestDrainOne_TeeOnlyNoLineSplit(t *testing.T) {
	var tee bytes.Buffer
	cfg := &startConfig{}
	p := &RunningProcess{}
	cfg.drainOne(context.Background(), p, StreamStderr,
		strings.NewReader("raw\r\noutput"), &tee, nil)
	if tee.String() != "raw\r\noutput" {
		t.Fatalf("tee = %q, want verbatim", tee.String())
	}
}

func TestEmit_DropNewestCountsDrops(t *testing.T) {
	var dropped int64
	ch := make(chan Line, 1)
	sink := lineSink{id: StreamStdout, lines: ch, overflow: OverflowDropNewest, dropped: &dropped}
	for i := 0; i < 5; i++ {
		sink.emit("x")
	}
	if dropped != 4 { // 1 buffered, 4 dropped
		t.Fatalf("dropped = %d, want 4", dropped)
	}
	if len(ch) != 1 {
		t.Fatalf("channel holds %d, want 1", len(ch))
	}
}

// TestEmit_BlockReleasedByCtx proves a blocked OverflowBlock send is freed when
// the start context is cancelled, so a drain can never hang forever. A
// teardown-released line is lost but not counted as a policy drop.
func TestEmit_BlockReleasedByCtx(t *testing.T) {
	var dropped int64
	ctx, cancel := context.WithCancel(context.Background())
	cancel()              // already done
	ch := make(chan Line) // unbuffered, no reader → a send would block
	sink := lineSink{id: StreamStdout, lines: ch, overflow: OverflowBlock, dropped: &dropped, ctxDone: ctx.Done()}
	assertReturns(t, func() { sink.emit("x") }) // must return via the ctx branch
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 (teardown release is not a policy drop)", dropped)
	}
}

// TestEmit_BlockReleasedByStop proves a blocked OverflowBlock send is also freed
// by the teardown stop signal (Group.Close / Kill), not only by ctx cancel.
func TestEmit_BlockReleasedByStop(t *testing.T) {
	var dropped int64
	stop := make(chan struct{})
	close(stop)
	ch := make(chan Line) // unbuffered, no reader → a send would block
	sink := lineSink{id: StreamStdout, lines: ch, overflow: OverflowBlock, dropped: &dropped, stop: stop}
	assertReturns(t, func() { sink.emit("x") }) // must return via the stop branch
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 (teardown release is not a policy drop)", dropped)
	}
}

// assertReturns fails if fn does not return promptly — used to prove emit is
// released (by ctx / stop) rather than blocking forever.
func assertReturns(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked — teardown signal did not release it")
	}
}

func TestStreamID_String(t *testing.T) {
	if StreamStdout.String() != "stdout" || StreamStderr.String() != "stderr" {
		t.Fatalf("StreamID.String mismatch: %q / %q", StreamStdout, StreamStderr)
	}
}

func TestLines_NotEnabledIsClosed(t *testing.T) {
	p := &RunningProcess{} // streaming not enabled
	select {
	case _, ok := <-p.Lines():
		if ok {
			t.Fatal("Lines() on a non-streaming process yielded a value")
		}
	default:
		t.Fatal("Lines() on a non-streaming process should be closed, not blocking")
	}
}

// upperReader is a trivial decoder for tests: it upper-cases ASCII bytes.
type upperReader struct{ r io.Reader }

func (u *upperReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	for i := 0; i < n; i++ {
		if c := p[i]; c >= 'a' && c <= 'z' {
			p[i] = c - 32
		}
	}
	return n, err
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package processkit

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"sync/atomic"
)

// StreamID identifies which standard stream a [Line] came from.
type StreamID uint8

const (
	// StreamStdout marks a line that came from the process's standard output.
	StreamStdout StreamID = iota
	// StreamStderr marks a line that came from the process's standard error.
	StreamStderr
)

// String renders the stream as "stdout" or "stderr".
func (s StreamID) String() string {
	switch s {
	case StreamStdout:
		return "stdout"
	case StreamStderr:
		return "stderr"
	default:
		return "stream?"
	}
}

// Line is one line of streamed output, tagged with the stream it came from. The
// trailing newline (and a preceding carriage return) is stripped from Text.
type Line struct {
	Stream StreamID
	Text   string
}

// OverflowPolicy decides what a streamed line channel does when it is full and
// the consumer is not keeping up. See [OnOverflow].
type OverflowPolicy uint8

const (
	// OverflowBlock applies backpressure: a full channel blocks the drain, which
	// eventually blocks the process on a full OS pipe. No line is lost. This is the
	// default. Cancelling the start context always releases a blocked drain.
	OverflowBlock OverflowPolicy = iota
	// OverflowDropNewest drops an incoming line when the channel is full rather
	// than blocking, so a slow consumer can never stall the process. The number of
	// dropped lines is reported by [RunningProcess.DroppedLines].
	OverflowDropNewest
)

// defaultBufferLines is the capacity of the [RunningProcess.Lines] channel when
// [BufferLines] is not given.
const defaultBufferLines = 1024

// defaultMaxLineBytes bounds in-memory accumulation for a single line that
// carries no newline, so a pathological stream cannot exhaust memory. A line
// longer than this is emitted in fragments rather than aborting the stream.
const defaultMaxLineBytes = 1 << 20 // 1 MiB

// startConfig is the resolved set of per-process options for [Group.Start].
type startConfig struct {
	stdin     io.Reader
	stdoutTee io.Writer
	stderrTee io.Writer
	onStdout  func(string)
	onStderr  func(string)

	streamLines  bool
	bufferLines  int
	overflow     OverflowPolicy
	decoder      func(io.Reader) io.Reader
	maxLineBytes int
}

// WithStdin feeds the process's standard input from r. For interactive input,
// pass the read end of an [io.Pipe] and write to its write end over time; close
// the write end to signal EOF. This is a [Group.Start] option; to feed the head
// of a chain, use the [Pipeline.WithStdin] method instead.
//
// You must eventually close (or exhaust) r once the process no longer needs input.
// If r is not an *os.File, a background goroutine copies it to the process, and a
// process that exits while that copy is blocked on r will stall [RunningProcess.Wait]
// for up to a few seconds before the pipe is force-closed; an r that never reaches
// EOF leaks that copy goroutine past [Group.Close], which cannot reach it.
func WithStdin(r io.Reader) StartOption {
	return func(c *startConfig) { c.stdin = r }
}

// WithStdout mirrors the process's standard output, verbatim and undecoded, to w
// as it is produced — for forwarding to os.Stdout, a log file, or a
// [bytes.Buffer] to capture the full output alongside streaming.
func WithStdout(w io.Writer) StartOption {
	return func(c *startConfig) { c.stdoutTee = w }
}

// WithStderr mirrors the process's standard error, verbatim and undecoded, to w
// as it is produced. Configuring any stderr consumer also drains stderr in the
// background, so the process can't deadlock on a full stderr pipe while you read
// only stdout.
func WithStderr(w io.Writer) StartOption {
	return func(c *startConfig) { c.stderrTee = w }
}

// OnStdoutLine registers a callback invoked for each line of standard output, in
// order, on a background goroutine. The callback runs inline with the drain, so a
// slow callback applies backpressure to the process. It MUST return: unlike a
// blocked [RunningProcess.Lines] send (which cancelling the context or closing the
// group releases), a callback stuck forever cannot be interrupted and will pin the
// process's drain goroutine. For cancel-safe consumption, prefer [StreamLines].
func OnStdoutLine(fn func(string)) StartOption {
	return func(c *startConfig) { c.onStdout = fn }
}

// OnStderrLine registers a callback invoked for each line of standard error, in
// order, on a background goroutine. See [OnStdoutLine] for the blocking contract.
func OnStderrLine(fn func(string)) StartOption {
	return func(c *startConfig) { c.onStderr = fn }
}

// StreamLines enables the merged line channel returned by [RunningProcess.Lines],
// carrying both stdout and stderr lines tagged by [StreamID] in arrival order.
// The channel is closed once both streams reach EOF. You must drain it until it
// closes (or cancel the start context), otherwise — under the default
// [OverflowBlock] — a full channel will stall the process.
func StreamLines() StartOption {
	return func(c *startConfig) { c.streamLines = true }
}

// BufferLines sets the capacity of the [RunningProcess.Lines] channel (default
// 1024). A value <= 0 selects the default — the channel cannot be made unbuffered
// this way. It has no effect unless [StreamLines] is also given.
func BufferLines(n int) StartOption {
	return func(c *startConfig) { c.bufferLines = n }
}

// OnOverflow sets what the [RunningProcess.Lines] channel does when it is full
// (default [OverflowBlock]). It governs the channel only — the [OnStdoutLine] /
// [OnStderrLine] callbacks always apply backpressure and never drop. It has no
// effect unless [StreamLines] is also given.
func OnOverflow(p OverflowPolicy) StartOption {
	return func(c *startConfig) { c.overflow = p }
}

// WithDecoder transforms each output stream before it is split into lines —
// the seam for non-UTF-8 console output. Pass a function that wraps a byte reader
// in a decoding reader (for example one from golang.org/x/text/encoding, which
// this package does not depend on). It applies to the line callbacks and the
// [RunningProcess.Lines] channel, not to the raw [WithStdout] / [WithStderr] tees.
func WithDecoder(d func(io.Reader) io.Reader) StartOption {
	return func(c *startConfig) { c.decoder = d }
}

// WithMaxLineBytes bounds how many bytes a single line may accumulate before it
// is emitted as a fragment (default 1 MiB), so a stream with no newline cannot
// exhaust memory. It does not abort the stream.
func WithMaxLineBytes(n int) StartOption {
	return func(c *startConfig) { c.maxLineBytes = n }
}

// resolve fills in defaults for the zero-valued knobs.
func (c *startConfig) resolve() {
	if c.bufferLines <= 0 {
		c.bufferLines = defaultBufferLines
	}
	if c.maxLineBytes <= 0 {
		c.maxLineBytes = defaultMaxLineBytes
	}
}

// wantsStdout reports whether standard output needs a pipe (a line consumer or a
// tee is configured); otherwise it is left discarded.
func (c *startConfig) wantsStdout() bool {
	return c.streamLines || c.onStdout != nil || c.stdoutTee != nil
}

// wantsStderr reports whether standard error needs a pipe.
func (c *startConfig) wantsStderr() bool {
	return c.streamLines || c.onStderr != nil || c.stderrTee != nil
}

// lineSink dispatches decoded lines to the per-stream callback and the merged
// channel. The zero value (nil callback, nil channel) is a valid no-op sink.
type lineSink struct {
	id       StreamID
	onLine   func(string)
	lines    chan<- Line
	overflow OverflowPolicy
	dropped  *int64
	ctxDone  <-chan struct{} // start context cancelled
	stop     <-chan struct{} // group/process torn down (Close / Kill)
}

// wantsLines reports whether this sink splits its stream into lines at all (vs. a
// tee-only stream that is copied through without line splitting).
func (s lineSink) wantsLines() bool { return s.onLine != nil || s.lines != nil }

// emit delivers one decoded line to the callback and the channel.
func (s lineSink) emit(text string) {
	if s.onLine != nil {
		s.onLine(text)
	}
	if s.lines == nil {
		return
	}
	l := Line{Stream: s.id, Text: text}
	if s.overflow == OverflowDropNewest {
		select {
		case s.lines <- l:
		default:
			atomic.AddInt64(s.dropped, 1)
		}
		return
	}
	// OverflowBlock: backpressure, but never a permanent stall — cancelling the
	// start context, or tearing the group/process down, releases the send (the
	// process is going away anyway), so a drain can't outlive its consumer. A
	// teardown-released line is lost but is NOT counted as a drop: DroppedLines
	// reports overflow-policy drops only, so it stays 0 under OverflowBlock.
	select {
	case s.lines <- l:
	case <-s.ctxDone:
	case <-s.stop:
	}
}

// readBufSize is the per-stream read buffer drainLines uses. It is independent of
// the (much larger) line cap, so streaming many processes stays memory-light.
const readBufSize = 32 * 1024

// drainLines reads src to EOF, splitting it into lines and dispatching each
// through sink. A line is delimited by '\n'; the trailing '\n' and a preceding
// '\r' are stripped. A newline-free run that reaches maxLineBytes is flushed as a
// fragment so memory stays bounded (a complete line is emitted whole, however
// long — it is already in memory). The final unterminated remainder, if any, is
// emitted too. The read buffer is small and fixed; only the cap governs memory.
func drainLines(src io.Reader, sink lineSink, maxLineBytes int) {
	if maxLineBytes <= 0 {
		maxLineBytes = defaultMaxLineBytes
	}
	buf := make([]byte, readBufSize)
	var acc []byte
	for {
		n, err := src.Read(buf)
		chunk := buf[:n]
		for len(chunk) > 0 {
			if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
				acc = append(acc, chunk[:i+1]...) // includes the '\n'
				sink.emit(trimLineEnd(acc))
				acc = acc[:0]
				chunk = chunk[i+1:]
				continue
			}
			acc = append(acc, chunk...)
			chunk = nil
			// Bound memory: flush whole cap-sized fragments of a newline-free run.
			// A mid-line fragment is emitted verbatim (no CR/LF trim — only a real
			// line terminator is stripped, and there is none here by construction).
			for len(acc) >= maxLineBytes {
				sink.emit(string(acc[:maxLineBytes]))
				acc = acc[maxLineBytes:]
			}
		}
		if err != nil { // EOF or read error: flush any remainder, then stop
			if len(acc) > 0 {
				sink.emit(trimLineEnd(acc))
			}
			return
		}
	}
}

// trimLineEnd drops a trailing "\n" and a preceding "\r" from a raw line.
func trimLineEnd(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return string(b)
}

// closedLineChan is returned by [RunningProcess.Lines] when streaming was not
// enabled, so a range over it terminates immediately instead of deadlocking.
var closedLineChan = func() chan Line {
	c := make(chan Line)
	close(c)
	return c
}()

// preparePipes wires stdin and, for any stream that has a consumer, swaps it for
// a pipe whose read end is returned for draining. It must run before ecmd.Start.
// On a later failure the caller closes the returned readers via closePipes.
func (c *startConfig) preparePipes(ecmd *exec.Cmd) (stdoutR, stderrR io.ReadCloser, err error) {
	ecmd.Stdin = c.stdin
	if c.wantsStdout() {
		if stdoutR, err = ecmd.StdoutPipe(); err != nil {
			return nil, nil, err
		}
	}
	if c.wantsStderr() {
		if stderrR, err = ecmd.StderrPipe(); err != nil {
			closePipes(stdoutR, nil)
			return nil, nil, err
		}
	}
	return stdoutR, stderrR, nil
}

// closePipes closes any pipe readers opened by preparePipes — used only on the
// Start error paths, where no drain goroutine will consume them.
func closePipes(stdoutR, stderrR io.ReadCloser) {
	if stdoutR != nil {
		_ = stdoutR.Close()
	}
	if stderrR != nil {
		_ = stderrR.Close()
	}
}

// launchDrains creates the merged line channel (when enabled) and starts the
// per-stream drain goroutines. It must run after ecmd.Start. p.drainWG gates
// reap() until every output pipe has been read to EOF.
func (c *startConfig) launchDrains(p *RunningProcess, ctx context.Context, stdoutR, stderrR io.ReadCloser) {
	if c.streamLines {
		p.lines = make(chan Line, c.bufferLines)
	}
	if stdoutR != nil {
		p.drainWG.Add(1)
		go func() {
			defer p.drainWG.Done()
			c.drainOne(ctx, p, StreamStdout, stdoutR, c.stdoutTee, c.onStdout)
		}()
	}
	if stderrR != nil {
		p.drainWG.Add(1)
		go func() {
			defer p.drainWG.Done()
			c.drainOne(ctx, p, StreamStderr, stderrR, c.stderrTee, c.onStderr)
		}()
	}
}

// drainOne drives one output stream to EOF: it mirrors raw bytes to tee, then
// either splits the (optionally decoded) stream into lines for the callback and
// the merged channel, or — when no line consumer is configured — just copies it
// through. Reading to EOF is what lets the process exit and reap() proceed.
func (c *startConfig) drainOne(ctx context.Context, p *RunningProcess, id StreamID, pipe io.Reader, tee io.Writer, onLine func(string)) {
	sink := lineSink{
		id:       id,
		onLine:   onLine,
		lines:    p.lines,
		overflow: c.overflow,
		dropped:  &p.dropped,
		ctxDone:  ctx.Done(),
		stop:     p.stop,
	}
	if !sink.wantsLines() {
		// Tee-only or a pure background drain: copy raw bytes, never split.
		if tee != nil {
			_, _ = io.Copy(tee, pipe)
		} else {
			_, _ = io.Copy(io.Discard, pipe)
		}
		return
	}
	src := pipe
	if tee != nil {
		src = io.TeeReader(pipe, tee) // mirror raw bytes as the line path consumes them
	}
	if c.decoder != nil {
		src = c.decoder(src) // decode after teeing, so the tee stays verbatim
	}
	drainLines(src, sink, c.maxLineBytes)
}

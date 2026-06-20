# processkit-go

Kernel-backed, no-orphan child-process management for Go — a native implementation
of the [processkit](https://github.com/ZelAnton/ProcessKit-rs) model.

Every process you start — and everything *it* spawns — runs inside a kill-on-drop
OS container (a Windows **Job Object**, or a POSIX **process group**), so no
descendant ever outlives your run. Capture output, read exit codes, set timeouts,
and cancel through `context.Context` — with typed, `errors.Is`/`errors.As`-friendly
errors.

> **Status:** early (v0.x). The API is still taking shape and is **not yet frozen**.

## Requirements

- Go 1.25 or later
- Windows or Unix (Linux, macOS, the BSDs)

## Installation

```sh
go get github.com/ZelAnton/processkit-go
```

## Usage

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ZelAnton/processkit-go"
)

func main() {
	ctx := context.Background()

	// Run-and-capture: a non-zero exit is data, not an error.
	res, err := processkit.Command("git", "rev-parse", "HEAD").Output(ctx)
	if err != nil {
		log.Fatal(err) // spawn failure, cancelled context, …
	}
	fmt.Println(res.Stdout(), res.Outcome())

	// Require success and get trimmed stdout directly.
	version, err := processkit.Command("go", "version").Run(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(version)
}
```

Pick the verb that fits: `Output` (the full `Result`), `Run` (trimmed stdout, must
succeed), `ExitCode` (the code), or `Probe` (a yes/no predicate). Bound a run with
`.WithTimeout(d)` and tear the whole tree down by cancelling the `context`.

For several processes that must die together — a server and its helpers — use a
`Group`, a shared kill-on-drop container:

```go
group, err := processkit.NewGroup()
if err != nil {
	log.Fatal(err)
}
defer group.Close() // reaps the whole tree, grandchildren included

server, err := group.Start(ctx, processkit.Command("my-server"))
if err != nil {
	log.Fatal(err)
}
_ = server
// ... use the server; group.Shutdown(ctx) ends it gracefully (SIGTERM → grace →
// SIGKILL on Unix; an atomic kill on Windows).
```

### Streaming & interactive I/O

A group-started process can stream its output line by line. `StreamLines` enables
a single merged channel of stdout **and** stderr lines, each tagged with its
stream; it closes when the process has produced all its output (or when you cancel
the context):

```go
proc, err := group.Start(ctx, processkit.Command("journalctl", "-f"),
	processkit.StreamLines())
if err != nil {
	log.Fatal(err)
}
for line := range proc.Lines() {
	fmt.Printf("[%s] %s\n", line.Stream, line.Text)
}
```

The pieces compose — mix and match per start:

- **Callbacks:** `OnStdoutLine(fn)` / `OnStderrLine(fn)` invoke `fn` per line.
- **Tees:** `WithStdout(w)` / `WithStderr(w)` mirror the raw bytes to any
  `io.Writer` (pass a `bytes.Buffer` to capture the full output while streaming).
- **Interactive stdin:** `WithStdin(r)` feeds input from any `io.Reader`; pass the
  read end of an `io.Pipe` to drive a child over time.
- **Backpressure:** the channel is bounded (`BufferLines(n)`); by default a slow
  reader applies backpressure. `OnOverflow(OverflowDropNewest)` instead drops
  lines (counted by `proc.DroppedLines()`) so a slow reader never stalls the child.
- **Encoding:** `WithDecoder(fn)` wraps each stream before line-splitting — plug in
  a `golang.org/x/text/encoding` reader for non-UTF-8 console output (no extra
  dependency is pulled into this module).

### Pipelines

Chain commands shell-free with `Pipe` — each stage's stdout feeds the next stage's
stdin over a real OS pipe, and the whole chain runs in one kill-on-drop container:

```go
// grep error log.txt | sort | uniq -c
counts, err := processkit.Pipe(
	processkit.Command("grep", "error", "log.txt"),
	processkit.Command("sort"),
	processkit.Command("uniq", "-c"),
).Run(ctx)
```

The verbs mirror a single command (`Output` / `Run` / `ExitCode` / `Probe`) and
return one `Result` folded by **pipefail attribution**: the captured stdout is
always the last stage's, while the program, stderr, and exit code are the **first
failing** stage's. Feed the head of the chain with `.WithStdin(r)` and bound the
whole chain with `.WithTimeout(d)`. For the `producer | head` pattern — where the
producer is killed by `SIGPIPE` once the consumer stops reading — mark the
producer `.WithUncheckedInPipe()` so it doesn't fail the chain.

### Retry

Replay a failed run to success with `WithRetry` — you supply a classifier deciding
which failures are worth another try (there is no default):

```go
out, err := processkit.Command("curl", "-sf", "https://example.com").
	WithTimeout(10 * time.Second).
	WithRetry(5, time.Second, func(err error) bool { // up to 5 attempts, 1s apart
		return errors.Is(err, processkit.ErrTimeout) // only retry timeouts
	}).
	Run(ctx)
```

`WithRetry(maxAttempts, backoff, retryIf)` runs at most `maxAttempts` times total,
sleeping `backoff` between tries, stopping on the first success, the first
non-retryable failure, or the budget — returning the last error. A cancelled
context is terminal (never retried). It applies to the success-requiring verbs
(`Run` / `ExitCode` / `Probe`). For transient low-level spawn failures, the
`IsTransient` helper is a ready-made classifier. This is **replay-to-success**;
to keep a long-running process *alive* across crashes, use `Supervise` instead.

### Readiness probes

A group-started process can be probed for readiness — and a probe **never kills**
the process, so if it isn't ready you decide what to do:

```go
server, _ := group.Start(ctx, processkit.Command("my-server"), processkit.StreamLines())

// Any one of:
line, err := server.WaitForLine(ctx, func(s string) bool {     // a log line appears
	return strings.Contains(s, "listening")
}, 10*time.Second)
err = server.WaitForPort(ctx, "127.0.0.1:8080", 10*time.Second) // a TCP port accepts
err = server.WaitFor(ctx, func(ctx context.Context) bool {      // a custom check passes
	return healthCheck(ctx) == nil
}, 10*time.Second)
```

On the deadline (or if the process exits first) a probe returns a `*NotReadyError`
(`errors.Is(err, processkit.ErrNotReady)`) — **distinct from `ErrTimeout`**, which
is the run's own deadline that *does* tear the tree down. `WaitForLine` watches the
merged stdout/stderr stream, so the process must be started with `StreamLines()`.

### Supervision

Keep a command alive with `Supervise` — it re-runs the command on a crash with
capped-exponential backoff (and jitter), until a stop condition is met:

```go
outcome, err := processkit.Supervise(processkit.Command("my-worker")).
	WithMaxRestarts(10).               // give up after 10 restarts
	WithBackoff(time.Second, 2).       // 1s, 2s, 4s, … capped at WithMaxBackoff (30s)
	Run(ctx)
// outcome.Stopped is StoppedPolicySatisfied / StoppedByPredicate / StoppedRestartsExhausted;
// outcome.Final holds the last run's Result; outcome.Restarts counts the re-runs.
```

A crash is any non-success run (a non-zero/rejected exit, a timeout, a signal
kill, or a spawn failure). The default policy (`RestartOnCrash`) stops on the
first clean run; `WithRestart(RestartAlways)` keeps re-running and
`WithRestart(RestartNever)` runs once. Stop on a custom condition with
`StopWhen(func(*Result) bool)`. For a flap-prone service, turn on the
failure-storm guard with `WithStormPause(d)` so a tight crash loop pauses instead
of hammering. Supervision is sequential — the whole process tree is reaped before
each restart — and a cancelled context ends it promptly.

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for the version history.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build/test instructions and
conventions. To report a security issue, follow [SECURITY.md](SECURITY.md) —
please do not open a public issue.

## License

This project is licensed under the [MIT License](LICENSE).

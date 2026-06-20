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

## Platform support

Containment is kernel-backed and the active mechanism is observable
(`Group.Mechanism()` / `Result.Mechanism()`) — never a silent downgrade. Where a
platform can't honour an operation, processkit says so (`ErrUnsupported` /
`ErrResourceLimit`) rather than skipping it silently.

| Capability | Windows | Linux | macOS / BSD |
| --- | --- | --- | --- |
| Containment (no-orphan teardown) | Job Object — `KILL_ON_JOB_CLOSE`, kernel-enforced even if the parent is hard-killed | process group¹ | process group¹ |
| Run/capture, streaming, pipelines, supervision, retries, probes, CLI client, record/replay | ✅ | ✅ | ✅ |
| Resource **stats** (`Group.Stats`, `RunningProcess.Profile`) | ✅ CPU + peak memory + count | per-process CPU/memory (`/proc`); group = count | count only (no `/proc`) |
| Resource **limits** (`WithMemoryMax` / `WithMaxProcesses` / `WithCPUQuota`) | ✅ enforced (Job Object) | ❌ `ErrResourceLimit`² | ❌ `ErrResourceLimit` |
| `Signal(SignalKill)` (atomic whole-tree kill) | ✅ | ✅ | ✅ |
| Other signals, `Suspend` / `Resume` | ❌ `ErrUnsupported` | ✅ | ✅ |
| `Adopt` an external process | ✅ | ✅ | ✅ |

¹ A POSIX process group is weaker than a Job Object: a child that calls `setsid`
escapes it, and teardown needs the parent to dispatch the kill (the `defer
group.Close()` path), so it is best-effort rather than kernel-enforced.

² A Linux **cgroup v2** backend that enforces limits (and strengthens containment)
is planned; until then a limit requested on Linux fails fast rather than going
unenforced. Even with cgroups, enforcement needs the process at the real cgroup-v2
root — not under systemd or in an ordinary container — so fail-fast is the common
path regardless.

**Teardown asymmetry worth knowing:** only the Windows Job Object's
`KILL_ON_JOB_CLOSE` survives a hard kill of the parent. On Unix the no-orphan
guarantee degrades to "holds as long as you don't forget `defer group.Close()`"
(plus `context` cancellation) — there is no RAII or finalizer to lean on, by design.

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

### Resource metrics

Read whole-tree resource usage from a `Group`, or profile one run:

```go
st, _ := group.Stats()                          // a snapshot
fmt.Println(st.ActiveProcesses())               // live process count
if cpu, ok := st.CPUTime(); ok { … }            // cumulative CPU (Job Object backend)

prof, _ := proc.Profile(ctx, 50*time.Millisecond) // sample one run to exit
fmt.Println(prof.Duration())                    // wall-clock, always available
if mem, ok := prof.PeakMemoryBytes(); ok { … }  // every optional metric returns ok
```

`Group.SampleStats(ctx, every)` returns a channel of snapshots. A metric a platform
can't read is reported as unavailable (the `ok` bool), never an error — and the
backends differ: the **Job Object** (Windows) reports count + CPU + peak memory;
the **process group** (Unix, no cgroup yet) reports the count only. Per-process
`RunningProcess.CPUTime` / `PeakMemoryBytes` work on Linux and Windows, not macOS.

### Resource limits

Cap the whole tree's resources when you create the group:

```go
group, err := processkit.NewGroup(
    processkit.WithMemoryMax(512*1024*1024), // 512 MiB across the tree
    processkit.WithMaxProcesses(64),         // at most 64 live processes
    processkit.WithCPUQuota(1.5),            // 1.5 cores' worth of CPU
)
if errors.Is(err, processkit.ErrResourceLimit) {
    // the active mechanism can't enforce a requested cap — handle, don't ignore
}
```

Limits are a **`Group`** facility — applied to the OS container at creation. There
is deliberately no per-run (`Cmd`) or per-`Pipe` cap; to bound a command's tree,
`Start` it into a limited group. Every cap bounds the **whole tree**, not one
process. Enforcement needs a real container: a **Windows Job Object** honours all
three. A mechanism with no whole-tree limit primitive does **not** silently ignore
a cap — `NewGroup` returns a `*ResourceLimitError` (matching `ErrResourceLimit`) so
you never get a group you believe is bounded but isn't. An invalid value (zero,
negative, non-finite) is rejected the same way. The honesty matrix:

| Backend                         | `WithMemoryMax` / `WithMaxProcesses` / `WithCPUQuota` |
| ------------------------------- | ----------------------------------------------------- |
| Windows Job Object              | ✅ enforced (job memory / active-process / CPU hard cap¹) |
| Linux / macOS / BSD (pgroup)    | ❌ `ErrResourceLimit` (no whole-tree cap primitive)   |

¹ The Windows CPU cap is expressed against *total* system CPU, so `WithCPUQuota` is
approximate (converted via the host core count) and saturates at 100%.

A Linux **cgroup v2** backend that enforces these is planned; until it lands, a cap
requested on Linux fails fast rather than going unenforced. (Even with cgroups, the
caps only apply where this process sits at the real cgroup-v2 root — not under
systemd or in an ordinary container — so fail-fast is the common path regardless.)

### Whole-tree process control

A `Group` can signal, pause, and adopt processes as a unit:

```go
group.Signal(processkit.SignalHup)   // reload the whole tree (SIGHUP)
group.Suspend(); group.Resume()      // freeze and thaw the tree
group.Adopt(cmd.Process)             // pull an externally-started process in
```

Operations a platform can't honour return `ErrUnsupported` explicitly — **never a
silent no-op**. The honesty matrix:

| Operation                  | Unix                | Windows            |
| -------------------------- | ------------------- | ------------------ |
| `Signal(SignalKill)`       | ✅ killpg / atomic  | ✅ TerminateJobObject |
| `Signal(other / RawSignal)`| ✅ killpg           | ❌ `ErrUnsupported`  |
| `Suspend` / `Resume`       | ✅ SIGSTOP / SIGCONT| ❌ `ErrUnsupported`  |
| `Adopt`                    | ✅ setpgid / solo   | ✅ AssignProcessToJobObject |
| `Members`                  | ✅                  | ✅                 |

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

### Typed CLI wrappers & test doubles

To wrap one CLI tool (git, gh, jj, …), build a `CliClient` — it injects the
program, defaults, and runner once, so your wrapper is just argument-building and
output-parsing, and is **mockable by construction**:

```go
type Git struct{ client *processkit.CliClient }

func NewGit() *Git {
	// WithEnv replaces the environment; AppendEnv adds to the inherited one:
	// .AppendEnv("GIT_TERMINAL_PROMPT=0")
	return &Git{client: processkit.NewClient("git")}
}
func (g *Git) CurrentBranch(ctx context.Context) (string, error) {
	return g.client.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
}
```

The `processkittest` package gives you ready-made fakes for the `ProcessRunner`
seam — no real subprocess in your tests:

```go
import "github.com/ZelAnton/processkit-go/processkittest"

scripted := processkittest.NewScriptedRunner().
	On([]string{"git", "rev-parse", "--abbrev-ref", "HEAD"}, processkittest.OK("main")).
	Fallback(processkittest.Fail(1, "unexpected command"))
git := &Git{client: processkit.NewClient("git").WithRunner(scripted)}
// git.CurrentBranch(ctx) now returns "main" with no `git` on the machine.
```

`ScriptedRunner` answers commands with canned `Reply`s (`OK` / `Fail` / `TimedOut`
/ `Signalled` / `Err` / `Pending`), and an unexpected command fails loudly.
`RecordingRunner` records the invocations a wrapper builds so you can assert on
them (`rec.OnlyCall().Args`). Writing your own runner? Build its results with
`processkit.NewResult`.

### Recording & replaying runs

`RecordReplayRunner` captures real runs to a JSON cassette once, then replays them
hermetically — no subprocess in CI:

```go
// Record once against the real tool:
rec := processkittest.Record("testdata/git.json", processkit.JobRunner{})
out, _ := processkit.Command("git", "--version").WithRunner(rec).Output(ctx)
_ = rec.Save() // writes the cassette (0600 on Unix)

// Replay everywhere else — identical results, no `git` needed:
rep, _ := processkittest.Replay("testdata/git.json")
out2, _ := processkit.Command("git", "--version").WithRunner(rep).Output(ctx)
// out2 matches out; an unrecorded command is a *CassetteMissError (errors.Is
// ErrCassetteMiss), distinct from a missing program, and never spawns anything.
```

A run is matched on program + args + working directory (environment is excluded, so
an irrelevant env difference can't cause a miss). A command recorded twice replays
in capture order, then repeats the last. Only runs that complete with a `Result` are
recorded — a spawn failure, not-found, or cancellation records nothing (a non-zero
exit, signal, or timeout *is* a result and is recorded). Replay is instant, not
timing-faithful (`Duration()` is zero).

**Secrets:** a cassette redacts environment **values** (it stores variable *names*
only), but **`program`, `args`, `cwd`, `stdout`, and `stderr` are stored verbatim**
and can carry secrets — a `--password=…` flag, a token echoed to output. Review a
fixture before committing it. On Unix the file is written owner-only (`0600`) and
the write refuses to follow a symlink; on Windows it inherits the directory ACL, so
keep the fixture directory restricted.

### Observability

Attach a [`log/slog`](https://pkg.go.dev/log/slog) logger with `WithLogger` — on a
`Cmd`, a `Pipeline`, a `Supervisor`, a `CliClient`, or a `Group` (the `WithLogger`
option). It is off by default.

```go
logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
out, _ := processkit.Command("ffmpeg", "-i", "in.mov", "out.mp4").
    WithLogger(logger).Output(ctx)
// {"level":"DEBUG","msg":"child spawned","program":"ffmpeg","pid":4242,"mechanism":"JobObject"}
// {"level":"DEBUG","msg":"process exited","program":"ffmpeg","outcome":"exited(0)","elapsed_ms":1830}
```

Events cover spawn/exit, timeout and cancellation, group teardown and graceful
shutdown, retries, and supervisor restarts / failure-storm pauses — at `Debug` for
ordinary lifecycle and `Warn` for anomalies. They carry the program name, pid,
mechanism, outcome, and durations — but **never the command's arguments,
environment, working directory, or output**, which routinely carry secrets. That
rule is structural: those values can't reach a log record.

The logger flows with the value it's set on: a `Cmd.WithLogger` command logs each
run even through `OutputAll` (set it per command — there is no batch-level logger),
and a `Group.WithLogger` group's started processes inherit it.

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for the version history.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for build/test instructions and
conventions. To report a security issue, follow [SECURITY.md](SECURITY.md) —
please do not open a public issue.

## License

This project is licensed under the [MIT License](LICENSE).

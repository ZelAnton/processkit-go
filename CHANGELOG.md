# Changelog

All notable changes to **processkit-go** are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Core run-and-capture API: `Command` plus the chainable `Cmd` builder
  (`WithArgs` / `WithDir` / `WithEnv` / `WithTimeout` / `WithOkCodes` /
  `WithRunner`, each returning a new `Cmd`), finished with a verb — `Output`,
  `Run`, `ExitCode`, or `Probe` — each taking a `context.Context`.
- `Result` and the three-way `Outcome` (exited / signalled / timed-out;
  `Signalled` is Unix-only), with `Mechanism` reporting the containment in effect.
- Per-run, kill-on-drop containment: a whole process tree (grandchildren included)
  dies with the run, via a Windows Job Object or a POSIX process group.
- Typed errors: sentinels (`ErrTimeout`, `ErrCancelled`, `ErrNotFound`,
  `ErrUnsupported`, `ErrNotReady`, `ErrResourceLimit`) and the rich `*ExitError`,
  `*NotFoundError`, `*StartError`, and `*CancelError` (error strings bound and
  sanitize child-controlled output).
- `ProcessRunner` interface + `JobRunner` — the dependency-injection / test seam.
- `Group` — an explicit, shared kill-on-drop container: `NewGroup`, `Start`,
  `Close`, graceful `Shutdown` (`ShutdownGrace` / `ShutdownOption`), `Processes`,
  `Mechanism`. `Close` reaps the whole tree; `Shutdown` does SIGTERM → grace →
  SIGKILL on Unix, an atomic kill on Windows.
- `RunningProcess` — a live handle (`Pid`, `Wait`, `Kill`) for group-started
  processes.
- Batch helpers: `WaitAny` / `WaitAll` over started processes, and
  concurrency-capped `OutputAll` (returning `BatchOutput`).
- Streaming & interactive I/O for `Group.Start`, via composable `StartOption`s:
  - `StreamLines` + `RunningProcess.Lines` — a merged stdout/stderr line channel
    of `Line{Stream StreamID; Text string}`, closed at EOF.
  - `OnStdoutLine` / `OnStderrLine` — per-line callbacks.
  - `WithStdin` (interactive input), `WithStdout` / `WithStderr` (verbatim tees).
  - Bounded-buffer policy: `BufferLines`, `OnOverflow` (`OverflowBlock` default /
    `OverflowDropNewest`), with `RunningProcess.DroppedLines`.
  - `WithDecoder` (non-UTF-8 console output; no new dependency) and
    `WithMaxLineBytes` (bounded memory on newline-free streams).
- Pipelines — shell-free `a | b | c` via `Pipe(stages ...*Cmd) *Pipeline`:
  - Stages wired stdout→stdin over real OS pipes; the whole chain runs in one
    shared kill-on-drop container and dies together.
  - Verbs mirror a command (`Output` / `Run` / `ExitCode` / `Probe`), returning a
    single `Result` folded by **pipefail attribution** (leftmost failing stage
    wins program/stderr/exit code; stdout is always the last stage's; a SIGPIPE
    victim is de-prioritised behind a real culprit).
  - `Pipeline.WithStdin` feeds the first stage; `Pipeline.WithTimeout` bounds the
    whole chain (kills all stages, keeps no partial output); per-stage
    `Cmd.WithTimeout` is honoured and attributed to that stage.
  - `Cmd.WithUncheckedInPipe` exempts a stage from attribution (the
    `producer | head` pattern).
  - `ErrTooFewStages` sentinel for a pipeline run with fewer than two stages.
    On a pipeline `Result`, `Program` / `Args` reflect the attributed stage.
- Supervision — keep a command alive via `Supervise(cmd *Cmd) *Supervisor`:
  - `RestartPolicy` (`RestartOnCrash` default / `RestartAlways` / `RestartNever`);
    a crash is any non-success run or a spawn failure.
  - Capped-exponential backoff (`WithBackoff` / `WithMaxBackoff`) with ±50%
    `WithJitter`; restart budget `WithMaxRestarts`.
  - Opt-in failure-storm guard (`WithStormPause` / `WithFailureDecay` /
    `WithFailureThreshold`): a tight crash loop pauses once the decaying failure
    score crosses the threshold.
  - `StopWhen(func(*Result) bool)` stop predicate (beats the policy); `WithRunner`
    DI seam. `Run(ctx)` returns a `SupervisionOutcome` (`Final`, `Restarts`,
    `Stopped` `StopReason`, `StormPauses`); sequential single-flight (the tree is
    reaped before each restart); a cancelled context or a terminal spawn failure
    is an error.
- Record/replay: `processkittest.RecordReplayRunner` captures real
  `Invocation → Result` pairs to a human-diffable JSON cassette (`Record(path,
  inner)` + `Save()`) and replays them hermetically (`Replay(path)`), over the same
  `ProcessRunner` seam. Matching is on program + args + working directory
  (environment excluded); a command recorded twice replays in capture order then
  repeats the last; an unrecorded command is a `*CassetteMissError` (matching the
  new `ErrCassetteMiss` sentinel) that never spawns a subprocess, distinct from a
  missing program. A cassette redacts environment **values** (stores names only),
  but `program` / `args` / `cwd` / `stdout` / `stderr` are stored verbatim — the
  file is written `0600` on Unix (refusing to follow a symlink), inherits the
  directory ACL on Windows, and a "review before committing" note is documented.
- Standard input for the capture verbs (`Output` / `Run` / `ExitCode` / `Probe`):
  `Cmd.WithStdin(io.Reader)` (one-shot, for streaming) and the re-readable
  `Cmd.WithStdinBytes([]byte)` / `Cmd.WithStdinString(string)` (fed afresh on every
  run, so they're safe with `WithRetry` and under a `Supervisor`). The `ProcessRunner`
  seam grew an `Invocation.Stdin` field. Live `Group.Start` keeps its own `WithStdin`
  start option and `Pipe` wires stdin along the chain (a stage/command `WithStdin` is
  ignored there); record/replay cassettes reject a command with stdin (its result
  isn't reproducible from the recorded program+args+dir key).
- Linux **cgroup v2** containment + limits backend. On Linux, processkit now prefers
  a cgroup v2 subtree over a process group: children are placed atomically at clone
  (`clone3` `CLONE_INTO_CGROUP`, kernel ≥ 5.7), teardown is the race-free
  `cgroup.kill`, suspend/resume is `cgroup.freeze`, and `Group.Mechanism()` reports
  `MechanismCgroupV2`. With the controllers delegated (the real cgroup-v2 root),
  `WithMemoryMax` / `WithMaxProcesses` / `WithCPUQuota` are now *enforced* on Linux
  (`memory.max` / `pids.max` / `cpu.max`). Where no cgroup can be made (no v2, no
  delegation, a read-only fs, an old kernel) processkit transparently falls back to a
  process group; where a cgroup exists but isn't the real root (systemd / a
  container) a requested limit still fails fast with `ErrResourceLimit`, never a
  silently-unbounded group.
- Pre-freeze API polish: `Group.Processes() []*RunningProcess` (a snapshot of the
  live process handles, so you can `WaitAll` over a group without retaining every
  `Start` handle, or read their pids via `RunningProcess.Pid`); `Cmd.AppendEnv` /
  `CliClient.AppendEnv` (add to
  the inherited environment, complementing the *replacing* `WithEnv` — the fix for
  the "inherit, plus set a few" footgun); and an `ErrStart` sentinel that
  `*StartError` matches via `errors.Is` (completing the sentinel-plus-typed-error
  pairing the rest of the taxonomy already has).
- Observability: an optional [`log/slog`](https://pkg.go.dev/log/slog) logger via
  `WithLogger`, off by default — on a `Cmd`, a `Pipeline`, a `Supervisor`, a
  `CliClient`, and a `Group` (the `WithLogger` `GroupOption`). Structured events
  cover spawn/exit, timeout/cancellation, group teardown / graceful shutdown,
  retries, and supervisor restarts / failure-storm pauses — `Debug` for lifecycle,
  `Warn` for anomalies. Events carry the program name, pid, mechanism, outcome, and
  durations, but **never** the command's arguments, environment, working directory,
  or output (the rule is structural — those values are not passed to any log call).
- Resource limits: `NewGroup` now takes `GroupOption`s — `WithMemoryMax(bytes)`,
  `WithMaxProcesses(n)`, `WithCPUQuota(cores)` — that cap the whole tree at creation
  (the call stays backward-compatible: `NewGroup()` with no options is unchanged).
  A **Windows Job Object** enforces all three (whole-job memory limit, active-process
  limit, CPU hard cap), and a **Linux cgroup v2** subtree enforces them where the
  controllers are delegated (see the cgroup bullet above). A mechanism with no
  whole-tree limit primitive — macOS/BSD, the Linux process-group fallback, or a
  Linux cgroup that isn't the real root — does not silently ignore a cap: `NewGroup`
  returns the new `*ResourceLimitError` (matching the `ErrResourceLimit` sentinel,
  now produced), as it does for an invalid value (zero / negative / non-finite).
- Resource metrics: `Group.Stats() (GroupStats, error)` (a whole-tree snapshot —
  `ActiveProcesses` always, plus `CPUTime` / `PeakMemoryBytes` on the Job Object
  backend), `Group.SampleStats(ctx, every)` (a channel of snapshots),
  `RunningProcess.CPUTime` / `PeakMemoryBytes` / `Elapsed`, and
  `RunningProcess.Profile(ctx, every) (RunProfile, error)` (samples one run to exit:
  duration, exit code, CPU, peak memory, sample count, `AvgCPU`). Optional metrics
  use an `ok` bool — a metric a platform can't read is never an error. Per-process
  metrics work on Linux (`/proc`) and Windows; the POSIX process-group backend
  reports only the count (no kernel accumulator without a cgroup).
- Whole-tree process control on `Group`: `Signal(Signal)`, `Suspend` / `Resume`,
  and `Adopt(*os.Process)`. A portable `Signal` type (`SignalTerm` / `SignalKill` /
  `SignalInt` / `SignalHup` / `SignalQuit` / `SignalUsr1` / `SignalUsr2` +
  `RawSignal(n)`). `SignalKill` works everywhere (the atomic whole-tree kill);
  other signals and `Suspend` / `Resume` are Unix-only and return `ErrUnsupported`
  on Windows (never a silent no-op). `Adopt` works on both (Job Object / setpgid),
  pulling an externally-started process into the group's containment.
- `CliClient` — a reusable core for typed wrappers around one CLI tool:
  `NewClient(program)` with copy-on-write `WithRunner` / `WithTimeout` / `WithEnv` /
  `WithDir` / `WithOkCodes` defaults, `Command(args...)` to build a sub-command, and
  `Run` / `Output` / `ExitCode` / `Probe(ctx, args...)` shortcuts. Mockable by
  construction (inject a fake runner).
- `processkittest` package — hermetic test doubles on the `ProcessRunner` seam:
  `ScriptedRunner` (canned `Reply`s via `On` / `OnSequence` / `When` / `Fallback`;
  unmatched commands fail loudly) and `RecordingRunner` (records invocations for
  `Calls` / `OnlyCall` assertions). `Reply` constructors: `OK` / `Fail` / `TimedOut`
  / `Signalled` / `Err` / `Pending` (+ `WithStdout` / `WithStderr`).
- Public `Result` / `Outcome` construction seam for custom runners and fakes:
  `NewResult(inv, outcome, stdout, stderr)` plus `Exited` / `Signalled` / `TimedOut`
  outcome constructors.
- Retry — `Cmd.WithRetry(maxAttempts, backoff, retryIf)` replays a failed run to
  success: it runs at most `maxAttempts` times total, sleeps a constant `backoff`
  between tries, and retries only while `retryIf func(error) bool` classifies the
  failure as retryable (no default classifier). Stops on the first success,
  non-retryable failure, or budget, returning the last error; a cancelled context
  is terminal and aborts the backoff. Applies to `Run` / `ExitCode` / `Probe` (not
  `Output`, nor a stage in a pipeline / under a supervisor). `IsTransient(err)` is
  a ready-made classifier for transient low-level spawn failures.
- Readiness probes on `RunningProcess` — `WaitForLine` (a matching output line,
  returned), `WaitForPort` (a TCP address accepts), and `WaitFor` (a custom
  predicate). Each takes a `context.Context` and a `within` deadline. A probe
  never kills the process: on the deadline, or if the process exits first, it
  returns a `*NotReadyError` (matching `ErrNotReady`, carrying the probe kind and
  last failure) — distinct from `ErrTimeout`. `WaitForLine` requires `StreamLines`.

### Changed
-

### Fixed
- `Group.Start` racing `Group.Close` no longer leaks the drain/reap goroutines or
  (on Unix) orphans the child: containment (`Assign`) and registration now happen
  under the group lock with a closed-group check, so a process is either torn down
  by the `Close` that snapshots it or refused (a `*StartError` wrapping a
  closed-group error) and torn down by `Start` itself.
- macOS/BSD: `Group.Close` / `Shutdown` no longer return a spurious
  "operation not permitted" when the only remaining group members are unreaped
  zombies (`killpg` returns `EPERM` there, where Linux returns `ESRCH`); the
  process-group teardown now treats it as the benign already-dead case.

[Unreleased]: https://github.com/ZelAnton/processkit-go/commits/main

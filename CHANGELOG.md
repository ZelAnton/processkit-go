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
  `Close`, graceful `Shutdown` (`ShutdownGrace` / `ShutdownOption`), `Members`,
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

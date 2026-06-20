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

### Changed
-

### Fixed
-

[Unreleased]: https://github.com/ZelAnton/processkit-go/commits/main

// Package processkit runs and supervises child processes with a kernel-backed,
// no-orphan guarantee: every process you start — and everything it spawns — lives
// in a kill-on-drop OS container (a Windows Job Object, or a POSIX process
// group), so no descendant outlives your program.
//
// Start every run with [Command]; the verb you finish with decides what you get
// back:
//
//   - [Cmd.Output]   — the full [Result]; a non-zero exit is data, never an error.
//   - [Cmd.Run]      — trimmed stdout as a string; a non-zero exit / timeout errors.
//   - [Cmd.ExitCode] — the exit code (a timeout or signal kill errors, not -1).
//   - [Cmd.Probe]    — a bool: exit 0 → true, 1 → false, anything else errors.
//
// A run is bounded by [Cmd.WithTimeout] (captured in the Result as
// [Outcome.TimedOut]) and by the [context.Context] passed to every verb
// (cancelling it, or its own deadline elapsing, is an error — see [ErrCancelled]).
// Errors are typed: match the sentinels with errors.Is ([ErrTimeout],
// [ErrCancelled], [ErrNotFound], …) and the rich [*ExitError] / [*NotFoundError]
// with errors.As.
//
// For several processes that must die together — a server and its helpers — use a
// [Group]: a shared kill-on-drop container. [Group.Start] runs each process into
// one OS container; [Group.Close] (use `defer`) reaps the whole tree. [Group.Shutdown]
// ends it gracefully (SIGTERM → grace → SIGKILL on Unix; an immediate atomic kill
// on Windows). [WaitAny] / [WaitAll] race or join started processes, and [OutputAll]
// runs a batch of commands with a concurrency cap.
//
// A group-started process can stream its output: pass [StreamLines] and range over
// [RunningProcess.Lines], a merged stdout/stderr channel of tagged [Line]s that
// closes at EOF. Per-line callbacks ([OnStdoutLine] / [OnStderrLine]), raw tees
// ([WithStdout] / [WithStderr]), interactive [WithStdin], a bounded-buffer policy
// ([BufferLines] / [OnOverflow]), and an encoding [WithDecoder] all compose as
// [StartOption]s on [Group.Start].
//
// The [ProcessRunner] interface is the dependency-injection and test seam: swap
// the real [JobRunner] for a fake to test command-running code with no subprocess.
//
// Only Windows and Unix (Linux, macOS, the BSDs) are supported.
package processkit

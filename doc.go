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
// To chain commands shell-free — a | b | c — use [Pipe]: each stage's stdout
// feeds the next stage's stdin over a real OS pipe, and the whole chain runs in
// one kill-on-drop container, so it lives and dies together. The verbs mirror
// [Cmd] and return one [Result] folded by *pipefail attribution*: the captured
// stdout is always the last stage's, while the program, stderr, and exit code are
// the first failing (non-exempt) stage's — preferring a real culprit over an
// upstream stage killed by SIGPIPE. A stage marked [Cmd.WithUncheckedInPipe] is
// exempt from blame (the `producer | head` pattern). [Pipeline.WithTimeout] bounds
// the whole chain.
//
// To keep a command alive, wrap it in a [Supervisor]: it re-runs the command on
// a crash with capped-exponential backoff (plus jitter, and an optional
// failure-storm guard) until a stop condition is met — a clean run, a
// [Supervisor.StopWhen] predicate, or an exhausted restart budget. Supervision is
// sequential and single-flight, so a command's whole tree is always reaped before
// a restart. [Supervisor.Run] returns a [SupervisionOutcome] describing why it
// stopped and how many restarts it took.
//
// The [ProcessRunner] interface is the dependency-injection and test seam: swap
// the real [JobRunner] for a fake to test command-running code with no subprocess.
// The [Cmd] verbs and [Supervisor] run through it; [Group.Start] and [Pipe] always
// run real processes (their containment is the point), so they can't be faked this way.
//
// Context cancellation is reported two ways, by design: every verb that owns the
// run — the [Cmd], [Pipeline], and [Supervisor] verbs — wraps it in a
// [*CancelError] (a rich typed error), while the live-handle observers
// ([RunningProcess.Wait], [WaitAny], [WaitAll]) return the bare context error, so
// you can errors.Is it against context.Canceled / context.DeadlineExceeded without
// unwrapping. Note also that
// the stream options ([WithStdin], [WithStdout], [StreamLines], …) are
// package-level functions, not [Cmd] methods, because they configure a live
// [Group.Start] rather than the capture builder.
//
// Only Windows and Unix (Linux, macOS, the BSDs) are supported.
package processkit

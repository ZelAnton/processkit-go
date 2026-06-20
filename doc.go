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
// [StartOption]s on [Group.Start]. Feeding standard input is a [Group.Start]
// ([WithStdin]) or [Pipeline.WithStdin] facility — the batch capture verbs
// ([Cmd.Output] etc.) take no stdin; to pipe a buffer into one command, make it the
// head of a [Pipe] (or start it in a group).
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
// A [Group] also gives whole-tree process control: [Group.Signal] sends a [Signal]
// to every member (only [SignalKill] is portable — the others are Unix-only and
// return [ErrUnsupported] on Windows), [Group.Suspend] / [Group.Resume] freeze and
// thaw the tree (Unix only), and [Group.Adopt] pulls an externally-started process
// into the group's containment. Operations a platform can't honour return
// [ErrUnsupported] explicitly — never a silent no-op.
//
// Resource usage is available too: [Group.Stats] (and the [Group.SampleStats]
// channel) report a whole-tree snapshot — live process count, and, on the Job
// Object backend, cumulative CPU and peak memory; [RunningProcess.Profile] samples
// one run over its lifetime into a [RunProfile]. Metrics a platform can't read are
// reported as unavailable (an ok bool), never an error.
//
// A group can also cap the whole tree's resources at creation: [NewGroup] takes
// [WithMemoryMax], [WithMaxProcesses], and [WithCPUQuota] (a Group-only facility —
// there is no per-run or per-[Pipe] cap; Start a command into a limited group to
// bound it). A Windows Job Object and a Linux cgroup v2 subtree enforce all three;
// but cgroup enforcement needs the process at the real cgroup-v2 root (not under
// systemd or in a container), and macOS/BSD have no whole-tree cap, so where a cap
// can't be enforced [NewGroup] fails with a [*ResourceLimitError] (matching
// [ErrResourceLimit]) rather than hand back a silently-unbounded group. An
// unenforced limit is no protection.
//
// A group-started process can be probed for readiness: [RunningProcess.WaitForLine]
// waits for a line of its output to match, [RunningProcess.WaitForPort] waits for a
// TCP address to accept connections, and [RunningProcess.WaitFor] polls a custom
// predicate. These wait for the process to become *ready* (and leave it running) —
// not for it to *exit*, which is [RunningProcess.Wait]. A probe never kills the
// process: if it isn't ready by the deadline you get a [*NotReadyError] (matching
// [ErrNotReady], distinct from [ErrTimeout]) and decide what to do next.
//
// To keep a command alive, wrap it in a [Supervisor]: it re-runs the command on
// a crash with capped-exponential backoff (plus jitter, and an optional
// failure-storm guard) until a stop condition is met — a clean run, a
// [Supervisor.StopWhen] predicate, or an exhausted restart budget. Supervision is
// sequential and single-flight, so a command's whole tree is always reaped before
// a restart. [Supervisor.Run] returns a [SupervisionOutcome] describing why it
// stopped and how many restarts it took. To replay one run to *success* — rather
// than keep a process alive — attach [Cmd.WithRetry] to a verb instead: it retries
// a failed run, with a classifier deciding which failures are worth another try.
//
// The [ProcessRunner] interface is the dependency-injection and test seam: swap
// the real [JobRunner] for a fake to test command-running code with no subprocess.
// The [Cmd] verbs and [Supervisor] run through it; [Group.Start] and [Pipe] always
// run real processes (their containment is the point), so they can't be faked this way.
//
// For observability, attach an optional [log/slog] logger with WithLogger — on a
// [Cmd], a [Pipeline], a [Supervisor], a [CliClient], or a [Group] (the [WithLogger]
// option). It is off by default. Events cover spawn and exit, timeout and
// cancellation, group teardown and graceful shutdown, retries, supervisor restarts
// and failure-storm pauses — at Debug for ordinary lifecycle and Warn for anomalies.
// They carry the program name, pid, mechanism, outcome, and durations, but NEVER
// the command's arguments, environment, working directory, or output, which
// routinely carry secrets.
//
// To build a reusable typed wrapper around one CLI tool (git, gh, …), use a
// [CliClient]: it injects the program, defaults, and runner once, so the wrapper is
// just argument-building and output-parsing — and is mockable by construction. The
// processkittest package provides ready-made fakes for the seam (a scripted runner,
// a recording runner, and a record/replay runner that captures real runs to a JSON
// cassette and replays them hermetically); a hand-written custom runner builds its
// [Result]s with [NewResult].
//
// Context cancellation is reported two ways, by design: every verb that owns the
// run — the [Cmd], [Pipeline], and [Supervisor] verbs — wraps it in a
// [*CancelError] (a rich typed error), while the live-handle observers
// ([RunningProcess.Wait], [WaitAny], [WaitAll], and the readiness probes) return
// the bare context error, so you can errors.Is it against context.Canceled /
// context.DeadlineExceeded without unwrapping. Note also that
// the stream options ([WithStdin], [WithStdout], [StreamLines], …) and the group
// limit options ([WithMemoryMax], …) are package-level functions, not [Cmd]
// methods, because they configure a live [Group.Start] / [NewGroup] rather than the
// capture builder.
//
// Only Windows and Unix (Linux, macOS, the BSDs) are supported.
package processkit

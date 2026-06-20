package processkit

import (
	"context"
	"strings"
	"time"
)

// Cmd describes a command to run: a program, its arguments, and run options.
// Build it with [Command] and the chainable WithX methods, then finish with a
// verb ([Cmd.Output], [Cmd.Run], [Cmd.ExitCode], [Cmd.Probe]).
//
// Each WithX method returns a new, independent *Cmd (copy-on-write), so a partly
// configured command is safe to reuse and branch:
//
//	base := processkit.Command("git").WithDir(repo)
//	status := base.WithArgs("status")  // base is unchanged
//	log := base.WithArgs("log")        // independent of status
type Cmd struct {
	program string
	args    []string
	dir     string
	env     []string
	okCodes []int
	timeout time.Duration
	runner  ProcessRunner

	uncheckedInPipe bool // exempt from a Pipeline's pipefail attribution
}

// Command starts building a command that runs program with args. Finish with a
// verb (Output / Run / ExitCode / Probe).
func Command(program string, args ...string) *Cmd {
	return &Cmd{program: program, args: append([]string(nil), args...)}
}

// clone returns a deep copy of c (slices copied), so WithX never mutates the
// receiver shared with another caller.
func (c *Cmd) clone() *Cmd {
	cp := *c
	cp.args = append([]string(nil), c.args...)
	cp.env = cloneEnv(c.env)
	cp.okCodes = append([]int(nil), c.okCodes...)
	return &cp
}

// cloneEnv copies env, preserving the nil-vs-empty distinction (nil inherits the
// parent's environment; a non-nil empty slice clears it).
func cloneEnv(env []string) []string {
	if env == nil {
		return nil
	}
	return append([]string{}, env...)
}

// WithArgs returns a copy of the command with additional arguments appended.
func (c *Cmd) WithArgs(args ...string) *Cmd {
	cp := c.clone()
	cp.args = append(cp.args, args...)
	return cp
}

// WithDir returns a copy of the command with the given working directory.
func (c *Cmd) WithDir(dir string) *Cmd {
	cp := c.clone()
	cp.dir = dir
	return cp
}

// WithEnv returns a copy of the command with the full environment set, replacing
// the inherited one. Each entry is "KEY=VALUE"; calling it with no entries runs
// with an *empty* environment (no PATH) — usually you want to pass through the
// vars the program needs.
func (c *Cmd) WithEnv(env ...string) *Cmd {
	cp := c.clone()
	cp.env = append([]string{}, env...) // non-nil even when empty: clears the env
	return cp
}

// WithTimeout returns a copy of the command bounded by d. At the deadline the
// process tree is killed and the [Result] reports [Outcome.TimedOut] — a timeout
// is captured in the result, not raised, until a success-requiring verb turns it
// into an error. (Cancelling the context you pass is different: that is an error.)
func (c *Cmd) WithTimeout(d time.Duration) *Cmd {
	cp := c.clone()
	cp.timeout = d
	return cp
}

// WithOkCodes returns a copy of the command whose listed exit codes count as
// success in addition to 0. Affects [Result.Success] and the success-requiring
// verbs, but not [Cmd.Probe].
func (c *Cmd) WithOkCodes(codes ...int) *Cmd {
	cp := c.clone()
	cp.okCodes = append([]int(nil), codes...)
	return cp
}

// WithRunner returns a copy of the command that executes through r — the
// dependency-injection seam for tests. The default is a [JobRunner].
func (c *Cmd) WithRunner(r ProcessRunner) *Cmd {
	cp := c.clone()
	cp.runner = r
	return cp
}

// WithUncheckedInPipe returns a copy of the command exempt from a [Pipeline]'s
// pipefail attribution: as a pipeline stage, its failure never blames the chain —
// a non-zero exit always, and for a non-final stage a signal (including the
// SIGPIPE it gets when a downstream stage stops reading) or its own per-stage
// timeout too. This is the tool for the `producer | head` pattern. A final stage
// is only forgiven its non-zero exit; a timeout or signal kill still surfaces.
// Outside a pipeline it has no effect.
func (c *Cmd) WithUncheckedInPipe() *Cmd {
	cp := c.clone()
	cp.uncheckedInPipe = true
	return cp
}

func (c *Cmd) invocation() Invocation {
	return Invocation{
		Program: c.program,
		Args:    append([]string(nil), c.args...),
		Dir:     c.dir,
		Env:     cloneEnv(c.env),
		OkCodes: append([]int(nil), c.okCodes...),
		Timeout: c.timeout,
	}
}

func (c *Cmd) run(ctx context.Context) (*Result, error) {
	r := c.runner
	if r == nil {
		r = JobRunner{}
	}
	return r.Output(ctx, c.invocation())
}

// Output runs the command and returns the full [Result]. A non-zero exit is data
// here, not an error; only a spawn failure, a cancelled context, or a context
// deadline errors.
func (c *Cmd) Output(ctx context.Context) (*Result, error) {
	return c.run(ctx)
}

// Run requires a successful exit and returns stdout as text with trailing
// whitespace trimmed. A non-zero exit, timeout, signal kill, or cancellation is
// an error.
func (c *Cmd) Run(ctx context.Context) (string, error) {
	res, err := c.run(ctx)
	if err != nil {
		return "", err
	}
	if err := res.Err(); err != nil {
		return "", err
	}
	return strings.TrimRight(res.Stdout(), " \t\r\n"), nil
}

// ExitCode runs the command and returns its exit code. A run with no exit code
// (a timeout or signal kill) is an error rather than a fabricated -1.
func (c *Cmd) ExitCode(ctx context.Context) (int, error) {
	res, err := c.run(ctx)
	if err != nil {
		return 0, err
	}
	code, ok := res.Code()
	if !ok {
		return 0, res.toExitError()
	}
	return code, nil
}

// Probe runs the command as a yes/no predicate: exit 0 → true, exit 1 → false,
// anything else (another code, a timeout, a signal kill) → error. OkCodes does
// not apply to a probe.
func (c *Cmd) Probe(ctx context.Context) (bool, error) {
	res, err := c.run(ctx)
	if err != nil {
		return false, err
	}
	code, ok := res.Code()
	if !ok {
		return false, res.toExitError()
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, res.toExitError()
	}
}

package processkit

import (
	"context"
	"log/slog"
	"time"
)

// CliClient is a small, reusable core for building a typed wrapper around one
// external CLI tool (git, jj, gh, …). It holds the program name, a [ProcessRunner],
// and per-client defaults (timeout, environment, working directory, ok-codes), and
// hands back pre-configured commands — so a wrapper reduces to a typed facade over
// its argument-building and output-parsing, with the process plumbing injected once.
//
// Because the runner is injectable, a wrapper built on a CliClient is mockable by
// construction: give it a fake runner (see the processkittest package) and no real
// subprocess runs. The idiomatic wrapper embeds a client:
//
//	type Git struct{ client *processkit.CliClient }
//
//	func NewGit() *Git { return &Git{client: processkit.NewClient("git")} }
//	func (g *Git) Status(ctx context.Context) (string, error) {
//		return g.client.Run(ctx, "status", "--porcelain")
//	}
//
// CliClient is a value built by chainable WithX methods (each returning a new,
// independent *CliClient).
type CliClient struct {
	base *Cmd
}

// NewClient starts a client for the given program, with a real [JobRunner] and no
// defaults. Layer defaults with the WithX methods.
func NewClient(program string) *CliClient {
	return &CliClient{base: Command(program)}
}

// WithRunner returns a copy of the client whose commands all run through r — the
// dependency-injection and test seam. The default is a [JobRunner].
func (c *CliClient) WithRunner(r ProcessRunner) *CliClient {
	return &CliClient{base: c.base.WithRunner(r)}
}

// WithTimeout returns a copy of the client whose commands default to the deadline
// d (a per-command [Cmd.WithTimeout] overrides it).
func (c *CliClient) WithTimeout(d time.Duration) *CliClient {
	return &CliClient{base: c.base.WithTimeout(d)}
}

// WithEnv returns a copy of the client whose commands run with the full
// environment env (each entry "KEY=VALUE"), *replacing* the inherited one — so
// include everything the tool needs (HOME, PATH, …), not just an override. For the
// common "inherit, plus set a few" case, pass
// append(os.Environ(), "GIT_TERMINAL_PROMPT=0").
func (c *CliClient) WithEnv(env ...string) *CliClient {
	return &CliClient{base: c.base.WithEnv(env...)}
}

// AppendEnv returns a copy of the client whose commands add env to the inherited
// environment (rather than replacing it, as [CliClient.WithEnv] does). See
// [Cmd.AppendEnv].
func (c *CliClient) AppendEnv(env ...string) *CliClient {
	return &CliClient{base: c.base.AppendEnv(env...)}
}

// WithDir returns a copy of the client whose commands default to the working
// directory dir (a per-command [Cmd.WithDir] overrides it).
func (c *CliClient) WithDir(dir string) *CliClient {
	return &CliClient{base: c.base.WithDir(dir)}
}

// WithOkCodes returns a copy of the client whose commands default to treating the
// listed exit codes as success in addition to 0.
func (c *CliClient) WithOkCodes(codes ...int) *CliClient {
	return &CliClient{base: c.base.WithOkCodes(codes...)}
}

// WithLogger returns a copy of the client whose commands emit structured
// [log/slog] lifecycle events (see [Cmd.WithLogger]). The default is no logging;
// pass nil to disable. Arguments and environment are never logged.
func (c *CliClient) WithLogger(logger *slog.Logger) *CliClient {
	return &CliClient{base: c.base.WithLogger(logger)}
}

// Command builds a command for a subcommand: the client's program and defaults
// with args appended. Chain more WithX on it to override a default for this one
// call, then finish with a verb — or use the [CliClient.Run] etc. shortcuts.
func (c *CliClient) Command(args ...string) *Cmd {
	return c.base.WithArgs(args...)
}

// Run runs the subcommand and returns trimmed stdout; a non-zero exit errors.
// Shortcut for c.Command(args...).Run(ctx).
func (c *CliClient) Run(ctx context.Context, args ...string) (string, error) {
	return c.Command(args...).Run(ctx)
}

// Output runs the subcommand and returns the full [Result] (a non-zero exit is
// data, not an error). Shortcut for c.Command(args...).Output(ctx).
func (c *CliClient) Output(ctx context.Context, args ...string) (*Result, error) {
	return c.Command(args...).Output(ctx)
}

// ExitCode runs the subcommand and returns its exit code.
func (c *CliClient) ExitCode(ctx context.Context, args ...string) (int, error) {
	return c.Command(args...).ExitCode(ctx)
}

// Probe runs the subcommand as a yes/no predicate (exit 0 → true, 1 → false).
func (c *CliClient) Probe(ctx context.Context, args ...string) (bool, error) {
	return c.Command(args...).Probe(ctx)
}

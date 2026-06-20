package processkit

import (
	"context"
	"testing"
)

// benchRunner is a no-subprocess [ProcessRunner] returning a fixed result. It
// isolates processkit's own per-call overhead (building the [Invocation], the verb
// wrappers) from the OS spawn cost, so the hermetic benchmarks measure only the
// library — the real-subprocess ones below show the total is OS-spawn-dominated.
type benchRunner struct{ res *Result }

func (r benchRunner) Output(context.Context, Invocation) (*Result, error) { return r.res, nil }

// BenchmarkInvocation measures building the [ProcessRunner] seam value (the
// allocation profile of a verb call's input).
func BenchmarkInvocation(b *testing.B) {
	c := Command("git", "rev-parse", "HEAD").WithDir("/repo").WithEnv("PATH=/usr/bin")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = c.invocation()
	}
}

// BenchmarkCmdOutput_Hermetic measures the per-call overhead of [Cmd.Output]
// through a fake runner — no subprocess, so this is pure library cost.
func BenchmarkCmdOutput_Hermetic(b *testing.B) {
	res := NewResult(Invocation{Program: "x"}, exited(0), []byte("output"), nil)
	c := Command("x", "y").WithRunner(benchRunner{res: res})
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.Output(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCmdRun_Hermetic also exercises the retry/extract wrapper (Run goes
// through retryRun even with no retry policy).
func BenchmarkCmdRun_Hermetic(b *testing.B) {
	res := NewResult(Invocation{Program: "x"}, exited(0), []byte("output\n"), nil)
	c := Command("x").WithRunner(benchRunner{res: res})
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.Run(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCmdOutput_Real spawns a trivial real child each iteration — the cost is
// dominated by the OS fork/exec + the kill-on-drop job setup/teardown, confirming
// the library is syscall-bound, not overhead-bound.
func BenchmarkCmdOutput_Real(b *testing.B) {
	if testing.Short() {
		b.Skip("real-subprocess benchmark")
	}
	exe := selfExe(b)
	env := helperEnv("exit", "PK_CODE=0")
	ctx := context.Background()
	// Warm up: the first spawn of the large test binary pays a one-time cold-start
	// (binary load) that would dominate a low b.N — measure the steady state.
	if _, err := Command(exe).WithEnv(env...).Output(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Command(exe).WithEnv(env...).Output(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGroupStartClose_Real measures a group's create → start one child →
// close-and-reap cycle.
func BenchmarkGroupStartClose_Real(b *testing.B) {
	if testing.Short() {
		b.Skip("real-subprocess benchmark")
	}
	exe := selfExe(b)
	env := helperEnv("exit", "PK_CODE=0")
	ctx := context.Background()
	if g, err := NewGroup(); err == nil { // warm up the binary cold-start
		_, _ = g.Start(ctx, Command(exe).WithEnv(env...))
		_ = g.Close()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g, err := NewGroup()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := g.Start(ctx, Command(exe).WithEnv(env...)); err != nil {
			_ = g.Close()
			b.Fatal(err)
		}
		_ = g.Close()
	}
}

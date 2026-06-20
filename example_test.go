package processkit_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/ZelAnton/processkit-go"
)

// These examples are compiled and rendered on pkg.go.dev. They have no Output
// comment, so `go test` checks they compile but does not run them (no subprocess).

func ExampleCommand() {
	ctx := context.Background()

	// Run-and-capture; a non-zero exit is data, not an error.
	res, err := processkit.Command("git", "rev-parse", "HEAD").Output(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Stdout(), res.Outcome())
}

func ExampleCmd_Run() {
	// Run requires success and returns trimmed stdout.
	version, err := processkit.Command("go", "version").Run(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(version)
}

func ExampleGroup() {
	ctx := context.Background()

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

	// ... use the server, then end it gracefully (SIGTERM → grace → SIGKILL on
	// Unix; an atomic kill on Windows):
	_ = group.Shutdown(ctx, processkit.ShutdownGrace(5*time.Second))
}

func ExampleCliClient() {
	ctx := context.Background()
	// A reusable core for a typed git wrapper: program + defaults injected once.
	// (Pass append(os.Environ(), "GIT_TERMINAL_PROMPT=0")... to WithEnv to add a
	// var without dropping the inherited environment.)
	git := processkit.NewClient("git").WithTimeout(10 * time.Second)

	branch, err := git.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(branch)
}

func ExampleCmd_WithRetry() {
	ctx := context.Background()
	// Retry a flaky fetch up to 5 times, 1s apart, but only when it times out.
	out, err := processkit.Command("curl", "-sf", "https://example.com").
		WithTimeout(10*time.Second).
		WithRetry(5, time.Second, func(err error) bool {
			return errors.Is(err, processkit.ErrTimeout)
		}).
		Run(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out)
}

func ExampleGroup_Stats() {
	ctx := context.Background()
	group, err := processkit.NewGroup()
	if err != nil {
		log.Fatal(err)
	}
	defer group.Close()
	_, _ = group.Start(ctx, processkit.Command("my-worker"))

	st, err := group.Stats()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d live processes\n", st.ActiveProcesses())
	if cpu, ok := st.CPUTime(); ok { // available on Windows; count-only on POSIX groups
		fmt.Printf("CPU: %s\n", cpu)
	}
}

func ExampleCmd_WithLogger() {
	// Structured slog events for the run's lifecycle (spawn, exit, timeout, …). The
	// program name, pid, and outcome are logged; arguments and environment never are.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	out, err := processkit.Command("git", "rev-parse", "HEAD").
		WithLogger(logger).
		Output(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Stdout())
}

func ExampleNewGroup_limits() {
	ctx := context.Background()

	// Cap the whole tree's resources. A Windows Job Object enforces all three; a
	// mechanism without a whole-tree limit primitive (every Unix backend today)
	// returns a *ResourceLimitError rather than a silently-unbounded group.
	group, err := processkit.NewGroup(
		processkit.WithMemoryMax(512*1024*1024), // 512 MiB
		processkit.WithMaxProcesses(64),
		processkit.WithCPUQuota(1.5), // 1.5 cores' worth
	)
	if errors.Is(err, processkit.ErrResourceLimit) {
		log.Printf("limits unenforceable here: %v", err)
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	defer group.Close()
	_, _ = group.Start(ctx, processkit.Command("my-worker"))
}

func ExampleGroup_Signal() {
	ctx := context.Background()
	group, err := processkit.NewGroup()
	if err != nil {
		log.Fatal(err)
	}
	defer group.Close()

	server, err := group.Start(ctx, processkit.Command("my-server"))
	if err != nil {
		log.Fatal(err)
	}
	_ = server

	// Ask the whole tree to reload its config (SIGHUP). SignalKill works on every
	// platform; the other signals are Unix-only and return ErrUnsupported on Windows.
	if err := group.Signal(processkit.SignalHup); err != nil {
		log.Println("reload not delivered:", err) // e.g. ErrUnsupported on Windows
	}

	// Pause and resume the whole tree (Unix; ErrUnsupported on Windows).
	_ = group.Suspend()
	_ = group.Resume()
}

func ExampleRunningProcess_WaitForPort() {
	ctx := context.Background()
	group, err := processkit.NewGroup()
	if err != nil {
		log.Fatal(err)
	}
	defer group.Close()

	server, err := group.Start(ctx, processkit.Command("my-server"))
	if err != nil {
		log.Fatal(err)
	}
	// Wait for the server to accept connections. A probe never kills the process —
	// if it isn't ready in time you get a *NotReadyError and decide what to do.
	if err := server.WaitForPort(ctx, "127.0.0.1:8080", 10*time.Second); err != nil {
		log.Fatal(err)
	}
	fmt.Println("server is accepting connections")
}

func ExampleSupervisor() {
	ctx := context.Background()

	// Keep a worker alive: restart it on a crash with exponential backoff, and
	// give up after 10 restarts.
	outcome, err := processkit.Supervise(processkit.Command("my-worker")).
		WithMaxRestarts(10).
		WithBackoff(time.Second, 2).
		Run(ctx)
	if err != nil {
		log.Fatal(err) // the caller's context was cancelled, or a run never started
	}
	fmt.Printf("supervision ended (%s) after %d restarts\n", outcome.Stopped, outcome.Restarts)
}

func ExamplePipe() {
	ctx := context.Background()

	// Shell-free `grep error log.txt | sort | uniq -c` — one kill-on-drop chain.
	// The captured stdout is the last stage's; a failure is attributed to the
	// first failing stage.
	counts, err := processkit.Pipe(
		processkit.Command("grep", "error", "log.txt"),
		processkit.Command("sort"),
		processkit.Command("uniq", "-c"),
	).Run(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(counts)
}

func ExampleGroup_streaming() {
	ctx := context.Background()

	group, err := processkit.NewGroup()
	if err != nil {
		log.Fatal(err)
	}
	defer group.Close()

	// Stream a child's output line by line over a merged channel.
	proc, err := group.Start(ctx,
		processkit.Command("journalctl", "-f"),
		processkit.StreamLines())
	if err != nil {
		log.Fatal(err)
	}

	// Range until the channel closes (the process produced all its output);
	// cancelling ctx tears the tree down and closes the channel early.
	for line := range proc.Lines() {
		if line.Stream == processkit.StreamStderr {
			fmt.Fprintln(log.Writer(), line.Text)
			continue
		}
		fmt.Println(line.Text)
	}
}

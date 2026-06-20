package processkit_test

import (
	"context"
	"fmt"
	"log"
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

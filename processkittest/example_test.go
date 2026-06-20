package processkittest_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	processkit "github.com/ZelAnton/processkit-go"
	"github.com/ZelAnton/processkit-go/processkittest"
)

// Git is the typed wrapper under test — it embeds a CliClient, so injecting a
// fake runner needs no real `git` on the machine.
type Git struct{ client *processkit.CliClient }

func (g *Git) CurrentBranch(ctx context.Context) (string, error) {
	return g.client.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
}

func ExampleScriptedRunner() {
	scripted := processkittest.NewScriptedRunner().
		On([]string{"git", "rev-parse", "--abbrev-ref", "HEAD"}, processkittest.OK("main\n")).
		Fallback(processkittest.Fail(1, "unexpected command"))

	git := &Git{client: processkit.NewClient("git").WithRunner(scripted)}
	branch, _ := git.CurrentBranch(context.Background())
	fmt.Println(branch)
	// Output: main
}

func ExampleRecordingRunner() {
	rec := processkittest.Replying(processkittest.OK(""))
	git := &Git{client: processkit.NewClient("git").WithRunner(rec)}
	_, _ = git.CurrentBranch(context.Background())

	// Assert on the command the wrapper built, with no real subprocess.
	fmt.Println(rec.OnlyCall().Args)
	// Output: [rev-parse --abbrev-ref HEAD]
}

func ExampleRecordReplayRunner() {
	dir, _ := os.MkdirTemp("", "cassette")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "git.json")

	// Record once. In real use the inner runner is a processkit.JobRunner that runs
	// the real tool; here a scripted stand-in keeps the example hermetic.
	inner := processkittest.NewScriptedRunner().
		On([]string{"git", "--version"}, processkittest.OK("git version 2.43.0"))
	rec := processkittest.Record(path, inner)
	_, _ = processkit.Command("git", "--version").WithRunner(rec).Output(context.Background())
	if err := rec.Save(); err != nil { // a failed Save means no cassette — don't ignore it
		log.Fatal(err)
	}

	// Replay — no runner of your own, identical result, no subprocess.
	rep, _ := processkittest.Replay(path)
	out, _ := processkit.Command("git", "--version").WithRunner(rep).Output(context.Background())
	fmt.Println(out.Stdout())
	// Output: git version 2.43.0
}

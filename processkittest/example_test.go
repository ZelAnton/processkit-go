package processkittest_test

import (
	"context"
	"fmt"

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

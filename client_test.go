package processkit_test

import (
	"context"
	"slices"
	"testing"
	"time"

	processkit "github.com/ZelAnton/processkit-go"
	"github.com/ZelAnton/processkit-go/processkittest"
)

func TestCliClient_RunThroughFakeRunner(t *testing.T) {
	scripted := processkittest.NewScriptedRunner().
		On([]string{"git", "status", "--porcelain"}, processkittest.OK(""))
	client := processkit.NewClient("git").WithRunner(scripted)

	out, err := client.Run(context.Background(), "status", "--porcelain")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "" {
		t.Fatalf("out = %q, want empty (clean)", out)
	}
}

func TestCliClient_DefaultsApplied(t *testing.T) {
	rec := processkittest.Replying(processkittest.OK("ok"))
	client := processkit.NewClient("git").
		WithTimeout(5 * time.Second).
		WithEnv("GIT_TERMINAL_PROMPT=0").
		WithDir("/work").
		WithRunner(rec)

	if _, err := client.Run(context.Background(), "status"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := rec.OnlyCall()
	if call.Program != "git" {
		t.Fatalf("program = %q, want git", call.Program)
	}
	if !slices.Equal(call.Args, []string{"status"}) {
		t.Fatalf("args = %q, want [status]", call.Args)
	}
	if call.Timeout != 5*time.Second {
		t.Fatalf("timeout = %v, want 5s (client default)", call.Timeout)
	}
	if call.Dir != "/work" {
		t.Fatalf("dir = %q, want /work", call.Dir)
	}
	if !slices.Contains(call.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("env = %q, want it to contain GIT_TERMINAL_PROMPT=0", call.Env)
	}
}

func TestCliClient_PerCallOverridesDefault(t *testing.T) {
	rec := processkittest.Replying(processkittest.OK(""))
	client := processkit.NewClient("git").WithTimeout(5 * time.Second).WithRunner(rec)

	// Override the default timeout for this one call.
	if _, err := client.Command("fetch").WithTimeout(time.Second).Output(context.Background()); err != nil {
		t.Fatalf("Output: %v", err)
	}
	if call := rec.OnlyCall(); call.Timeout != time.Second {
		t.Fatalf("timeout = %v, want 1s (per-call override)", call.Timeout)
	}
}

// fakeGit is the idiomatic typed-wrapper pattern: a struct embedding a CliClient.
type fakeGit struct{ client *processkit.CliClient }

func (g *fakeGit) CurrentBranch(ctx context.Context) (string, error) {
	return g.client.Run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
}

func TestCliClient_TypedWrapperPattern(t *testing.T) {
	scripted := processkittest.NewScriptedRunner().
		On([]string{"git", "rev-parse", "--abbrev-ref", "HEAD"}, processkittest.OK("main\n"))
	git := &fakeGit{client: processkit.NewClient("git").WithRunner(scripted)}

	branch, err := git.CurrentBranch(context.Background())
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	if branch != "main" { // Run trims the trailing newline
		t.Fatalf("branch = %q, want main", branch)
	}
}

func TestNewResult_BuildsAUsableResult(t *testing.T) {
	inv := processkit.Invocation{Program: "tool", Args: []string{"a", "b"}, OkCodes: []int{2}}
	res := processkit.NewResult(inv, processkit.Exited(2), []byte("out\n"), []byte("err\n"))
	if res.Program() != "tool" {
		t.Fatalf("program = %q", res.Program())
	}
	if c, ok := res.Code(); !ok || c != 2 {
		t.Fatalf("code = %v, want 2", res.Outcome())
	}
	if !res.Success() { // exit 2 is an accepted ok-code
		t.Fatal("want success (exit 2 is in OkCodes)")
	}
	if res.Stdout() != "out\n" || res.Stderr() != "err\n" {
		t.Fatalf("stdout/stderr = %q / %q", res.Stdout(), res.Stderr())
	}
}

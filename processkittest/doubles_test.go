package processkittest_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	processkit "github.com/ZelAnton/processkit-go"
	"github.com/ZelAnton/processkit-go/processkittest"
)

func inv(program string, args ...string) processkit.Invocation {
	return processkit.Invocation{Program: program, Args: args}
}

func TestScripted_OnMatch(t *testing.T) {
	sr := processkittest.NewScriptedRunner().
		On([]string{"git", "status"}, processkittest.OK("clean"))
	res, err := sr.Output(context.Background(), inv("git", "status", "--porcelain"))
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if res.Stdout() != "clean" {
		t.Fatalf("stdout = %q, want clean", res.Stdout())
	}
}

func TestScripted_UnmatchedFailsLoud(t *testing.T) {
	sr := processkittest.NewScriptedRunner()
	_, err := sr.Output(context.Background(), inv("rm", "-rf"))
	if !errors.Is(err, processkit.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for an unexpected command", err)
	}
}

func TestScripted_Fallback(t *testing.T) {
	sr := processkittest.NewScriptedRunner().Fallback(processkittest.Fail(3, "boom"))
	res, err := sr.Output(context.Background(), inv("anything"))
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if c, ok := res.Code(); !ok || c != 3 {
		t.Fatalf("code = %v, want 3", res.Outcome())
	}
	if res.Stderr() != "boom" {
		t.Fatalf("stderr = %q", res.Stderr())
	}
}

func TestScripted_FirstMatchWins(t *testing.T) {
	sr := processkittest.NewScriptedRunner().
		On([]string{"git", "log"}, processkittest.OK("specific")).
		On([]string{"git"}, processkittest.OK("general"))
	res, _ := sr.Output(context.Background(), inv("git", "log"))
	if res.Stdout() != "specific" {
		t.Fatalf("stdout = %q, want the first (more specific) rule", res.Stdout())
	}
}

func TestScripted_SequenceThenRepeatsLast(t *testing.T) {
	sr := processkittest.NewScriptedRunner().
		OnSequence([]string{"git", "pull"}, processkittest.Fail(1, ""), processkittest.OK("done"))
	codes := []int{}
	for i := 0; i < 3; i++ {
		res, _ := sr.Output(context.Background(), inv("git", "pull"))
		c, _ := res.Code()
		codes = append(codes, c)
	}
	if !slices.Equal(codes, []int{1, 0, 0}) { // 1, then 0, then the last repeats
		t.Fatalf("codes = %v, want [1 0 0]", codes)
	}
}

// TestScripted_SequenceDrivesRetry confirms a scripted sequence drives a retried
// command: each attempt takes the next reply.
func TestScripted_SequenceDrivesRetry(t *testing.T) {
	sr := processkittest.NewScriptedRunner().
		OnSequence([]string{"git", "pull"},
			processkittest.Fail(1, "net blip"),
			processkittest.Fail(1, "net blip"),
			processkittest.OK("Updated."))
	out, err := processkit.Command("git", "pull").
		WithRunner(sr).
		WithRetry(3, 0, func(error) bool { return true }).
		Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "Updated." {
		t.Fatalf("out = %q, want the third (successful) reply", out)
	}
}

func TestScripted_WhenPredicate(t *testing.T) {
	sr := processkittest.NewScriptedRunner().
		When(func(i processkit.Invocation) bool {
			return len(i.Args) > 0 && i.Args[0] == "push"
		}, processkittest.OK("pushed"))
	res, _ := sr.Output(context.Background(), inv("git", "push", "origin"))
	if res.Stdout() != "pushed" {
		t.Fatalf("stdout = %q", res.Stdout())
	}
}

func TestScripted_PendingResolvesOnCancel(t *testing.T) {
	sr := processkittest.NewScriptedRunner().Fallback(processkittest.Pending())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	_, err := sr.Output(ctx, inv("server"))
	if !errors.Is(err, processkit.ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled (pending resolves on cancel)", err)
	}
}

func TestScripted_ErrReply(t *testing.T) {
	sr := processkittest.NewScriptedRunner().
		Fallback(processkittest.Err(&processkit.NotFoundError{Program: "missing"}))
	_, err := sr.Output(context.Background(), inv("missing"))
	if !errors.Is(err, processkit.ErrNotFound) {
		t.Fatalf("err = %v, want the scripted NotFound", err)
	}
}

func TestReply_FailWithStdout(t *testing.T) {
	sr := processkittest.NewScriptedRunner().
		Fallback(processkittest.Fail(2, "stderr text").WithStdout("partial stdout"))
	res, _ := sr.Output(context.Background(), inv("x"))
	if res.Stdout() != "partial stdout" || res.Stderr() != "stderr text" {
		t.Fatalf("stdout/stderr = %q / %q", res.Stdout(), res.Stderr())
	}
}

func TestRecording_RecordsAndDelegates(t *testing.T) {
	rec := processkittest.Replying(processkittest.OK("hi"))
	out, err := processkit.Command("git", "status").WithRunner(rec).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "hi" {
		t.Fatalf("out = %q, want hi (delegated reply)", out)
	}
	call := rec.OnlyCall()
	if call.Program != "git" || !slices.Equal(call.Args, []string{"status"}) {
		t.Fatalf("recorded %q %q", call.Program, call.Args)
	}
}

func TestRecording_CallsInOrder(t *testing.T) {
	rec := processkittest.Replying(processkittest.OK(""))
	ctx := context.Background()
	_, _ = processkit.Command("git", "add", ".").WithRunner(rec).Run(ctx)
	_, _ = processkit.Command("git", "commit", "-m", "x").WithRunner(rec).Run(ctx)
	calls := rec.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if !slices.Equal(calls[0].Args, []string{"add", "."}) || !slices.Equal(calls[1].Args, []string{"commit", "-m", "x"}) {
		t.Fatalf("recorded args: %q then %q", calls[0].Args, calls[1].Args)
	}
}

func TestRecording_OnlyCallPanicsOnMismatch(t *testing.T) {
	rec := processkittest.Replying(processkittest.OK(""))
	defer func() {
		if recover() == nil {
			t.Fatal("OnlyCall should panic when there was not exactly one call")
		}
	}()
	_ = rec.OnlyCall() // zero calls → panic
}

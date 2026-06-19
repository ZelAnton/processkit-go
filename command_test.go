package processkit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRunner is a hermetic ProcessRunner double: it records the invocation and
// returns a canned result, so verb semantics are tested with no subprocess.
type fakeRunner struct {
	res *Result
	err error
	inv Invocation
}

func (f *fakeRunner) Output(_ context.Context, inv Invocation) (*Result, error) {
	f.inv = inv
	return f.res, f.err
}

func cmdWith(f *fakeRunner) *Cmd { return Command("p").WithRunner(f) }

func TestOutput_NonZeroExitIsData(t *testing.T) {
	f := &fakeRunner{res: &Result{program: "p", outcome: exited(3), stdout: []byte("hi")}}
	res, err := cmdWith(f).Output(context.Background())
	if err != nil {
		t.Fatalf("Output must not error on a non-zero exit: %v", err)
	}
	if c, ok := res.Code(); !ok || c != 3 {
		t.Fatalf("Code() = (%d, %v), want (3, true)", c, ok)
	}
	if res.Success() {
		t.Fatal("exit 3 should not be a success")
	}
}

func TestRun_TrimsAndRequiresSuccess(t *testing.T) {
	f := &fakeRunner{res: &Result{program: "p", outcome: exited(0), stdout: []byte("  hello \n")}}
	out, err := cmdWith(f).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "  hello" {
		t.Fatalf("Run trimmed = %q, want %q", out, "  hello")
	}

	f2 := &fakeRunner{res: &Result{program: "p", outcome: exited(1), stderr: "boom"}}
	_, err = cmdWith(f2).Run(context.Background())
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Run on a failure: want *ExitError, got %v", err)
	}
}

func TestExitCode(t *testing.T) {
	f := &fakeRunner{res: &Result{outcome: exited(7)}}
	code, err := cmdWith(f).ExitCode(context.Background())
	if err != nil || code != 7 {
		t.Fatalf("ExitCode = (%d, %v), want (7, nil)", code, err)
	}

	f2 := &fakeRunner{res: &Result{outcome: timedOut()}}
	if _, err := cmdWith(f2).ExitCode(context.Background()); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timed-out ExitCode: want ErrTimeout, got %v", err)
	}
}

func TestProbe(t *testing.T) {
	cases := []struct {
		code    int
		want    bool
		wantErr bool
	}{{0, true, false}, {1, false, false}, {2, false, true}}
	for _, tc := range cases {
		f := &fakeRunner{res: &Result{outcome: exited(tc.code)}}
		ok, err := cmdWith(f).Probe(context.Background())
		if (err != nil) != tc.wantErr || ok != tc.want {
			t.Fatalf("Probe(exit %d) = (%v, %v), want (%v, err=%v)", tc.code, ok, err, tc.want, tc.wantErr)
		}
	}
}

func TestOkCodes(t *testing.T) {
	// Plumbing: WithOkCodes reaches the invocation.
	f := &fakeRunner{res: &Result{outcome: exited(0)}}
	_, _ = Command("p").WithRunner(f).WithOkCodes(2, 3).Output(context.Background())
	if len(f.inv.OkCodes) != 2 || f.inv.OkCodes[0] != 2 {
		t.Fatalf("inv.OkCodes = %v, want [2 3]", f.inv.OkCodes)
	}
	// Semantics: an Ok code is a success.
	r := &Result{outcome: exited(2), okCodes: []int{2}}
	if !r.Success() {
		t.Fatal("exit 2 with OkCodes{2} should be a success")
	}
	if r.Err() != nil {
		t.Fatalf("Err on an OK code should be nil: %v", r.Err())
	}
}

func TestExitErrorMatchesErrTimeout(t *testing.T) {
	f := &fakeRunner{res: &Result{program: "p", outcome: timedOut(), stdout: []byte("partial")}}
	_, err := cmdWith(f).Run(context.Background())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want errors.Is ErrTimeout, got %v", err)
	}
	var ee *ExitError
	if !errors.As(err, &ee) || !ee.Outcome.TimedOut() {
		t.Fatalf("want a timed-out *ExitError, got %v", err)
	}
}

func TestInvocationPlumbing(t *testing.T) {
	f := &fakeRunner{res: &Result{outcome: exited(0)}}
	_, _ = Command("git", "status").
		WithRunner(f).
		WithArgs("--porcelain").
		WithDir("/repo").
		WithEnv("A=1", "B=2").
		WithTimeout(5 * time.Second).
		Output(context.Background())

	inv := f.inv
	if inv.Program != "git" {
		t.Fatalf("Program = %q", inv.Program)
	}
	if strings.Join(inv.Args, " ") != "status --porcelain" {
		t.Fatalf("Args = %v", inv.Args)
	}
	if inv.Dir != "/repo" {
		t.Fatalf("Dir = %q", inv.Dir)
	}
	if strings.Join(inv.Env, ",") != "A=1,B=2" {
		t.Fatalf("Env = %v", inv.Env)
	}
	if inv.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %v", inv.Timeout)
	}
}

func TestExitErrorSanitizesControlChars(t *testing.T) {
	ee := &ExitError{Program: "p", Outcome: exited(1), Stderr: "bad\x1b[31mred\x1b[0m output"}
	msg := ee.Error()
	if strings.ContainsRune(msg, 0x1b) {
		t.Fatalf("ANSI escape leaked into error string: %q", msg)
	}
	if !strings.Contains(msg, "exited with code 1") {
		t.Fatalf("error string missing exit info: %q", msg)
	}
}

// TestCmd_WithIsCopyOnWrite guards the documented copy-on-write contract: a
// partly-configured Cmd can be branched without the branches interfering.
func TestCmd_WithIsCopyOnWrite(t *testing.T) {
	base := Command("git").WithDir("/repo")
	a := base.WithArgs("status")
	b := base.WithArgs("log")

	f := &fakeRunner{res: &Result{outcome: exited(0)}}
	_, _ = a.WithRunner(f).Output(context.Background())
	if got := strings.Join(f.inv.Args, " "); got != "status" {
		t.Fatalf("a args = %q, want %q (b leaked into a)", got, "status")
	}
	_, _ = b.WithRunner(f).Output(context.Background())
	if got := strings.Join(f.inv.Args, " "); got != "log" {
		t.Fatalf("b args = %q, want %q", got, "log")
	}
}

// TestResult_AccessorsReturnCopies guards that Args/StdoutBytes hand out copies,
// so a caller mutating the returned slice can't corrupt the Result.
func TestResult_AccessorsReturnCopies(t *testing.T) {
	r := &Result{args: []string{"a"}, stdout: []byte("xy")}

	got := r.Args()
	got[0] = "MUT"
	if r.Args()[0] != "a" {
		t.Fatal("Args() must return a copy")
	}

	b := r.StdoutBytes()
	b[0] = 'Z'
	if r.StdoutBytes()[0] != 'x' {
		t.Fatal("StdoutBytes() must return a copy")
	}
}

func TestOutcomeString(t *testing.T) {
	sig := 9
	cases := []struct {
		got  Outcome
		want string
	}{
		{exited(0), "exited(0)"},
		{exited(3), "exited(3)"},
		{signalled(&sig), "signalled(9)"},
		{signalled(nil), "signalled"},
		{timedOut(), "timedOut"},
	}
	for _, tc := range cases {
		if got := tc.got.String(); got != tc.want {
			t.Errorf("Outcome.String() = %q, want %q", got, tc.want)
		}
	}
}

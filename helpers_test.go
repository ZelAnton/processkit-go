package processkit

import (
	"errors"
	"os/exec"
	"testing"
)

// resolveOutcome's precedence is guarantee-bearing: a run's own deadline wins over
// its exit status. The ProcessState arm is covered by the real-subprocess tests; the
// precedence and the nil-state fallback are pinned here.
func TestResolveOutcome(t *testing.T) {
	if oc := resolveOutcome(nil, true); !oc.TimedOut() {
		t.Errorf("deadlineFired → want timedOut, got %s", oc)
	}
	if oc := resolveOutcome(nil, false); oc.String() != "exited(0)" {
		t.Errorf("no deadline, no state → want exited(0), got %s", oc)
	}
}

func TestStartErr(t *testing.T) {
	if err := startErr("git", exec.ErrNotFound); !errors.Is(err, ErrNotFound) {
		t.Errorf("ErrNotFound → want *NotFoundError, got %v", err)
	}
	var nf *NotFoundError
	if !errors.As(startErr("git", exec.ErrNotFound), &nf) || nf.Program != "git" {
		t.Error("startErr should carry the program in a *NotFoundError")
	}
	if err := startErr("git", errors.New("boom")); !errors.Is(err, ErrStart) {
		t.Errorf("other cause → want *StartError, got %v", err)
	}
}

func TestResultVerbInterpreters(t *testing.T) {
	mk := func(o Outcome, stdout string) *Result {
		return NewResult(Invocation{Program: "x"}, o, []byte(stdout), nil)
	}
	// resultRun: trimmed stdout on success, error otherwise.
	if s, err := resultRun(mk(exited(0), "hi\n")); err != nil || s != "hi" {
		t.Errorf("resultRun(exit 0) = %q, %v; want \"hi\", nil", s, err)
	}
	if _, err := resultRun(mk(exited(2), "")); err == nil {
		t.Error("resultRun(exit 2) should error")
	}
	// resultExitCode: the code, or error when there is none.
	if c, err := resultExitCode(mk(exited(7), "")); err != nil || c != 7 {
		t.Errorf("resultExitCode(exit 7) = %d, %v; want 7, nil", c, err)
	}
	if _, err := resultExitCode(mk(timedOut(), "")); err == nil {
		t.Error("resultExitCode(timeout) should error (no code)")
	}
	// resultProbe: 0→true, 1→false, else→error.
	if ok, err := resultProbe(mk(exited(0), "")); err != nil || !ok {
		t.Errorf("resultProbe(0) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := resultProbe(mk(exited(1), "")); err != nil || ok {
		t.Errorf("resultProbe(1) = %v, %v; want false, nil", ok, err)
	}
	if _, err := resultProbe(mk(exited(2), "")); err == nil {
		t.Error("resultProbe(2) should error")
	}
}

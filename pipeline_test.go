package processkit

import "testing"

// These tests exercise the pure pipefail fold directly, with constructed stage
// outcomes — no subprocess. They mirror the conformance cases from the reference
// crate's pipefail unit tests.

func sig(n int) *int { return &n }

// fold is a tiny helper that runs pipefail with throwaway mechanism/duration.
func fold(stages ...stageOutcome) *Result {
	return pipefail(stages, []byte("LAST"), MechanismUnknown, 0)
}

func TestPipefail_AllCleanLastSpeaks(t *testing.T) {
	r := fold(
		stageOutcome{program: "a", outcome: exited(0)},
		stageOutcome{program: "b", outcome: exited(0)},
		stageOutcome{program: "c", outcome: exited(0)},
	)
	if !r.Success() {
		t.Fatalf("want success, got %v", r.Outcome())
	}
	if r.Program() != "c" {
		t.Fatalf("program = %q, want c (last stage speaks)", r.Program())
	}
	if r.Stdout() != "LAST" {
		t.Fatalf("stdout = %q, want the last stage's output", r.Stdout())
	}
}

func TestPipefail_LeftmostFailureAttributed(t *testing.T) {
	r := fold(
		stageOutcome{program: "a", outcome: exited(0)},
		stageOutcome{program: "b", outcome: exited(3), stderr: "b failed"},
		stageOutcome{program: "c", outcome: exited(5), stderr: "c failed"},
	)
	if c, ok := r.Code(); !ok || c != 3 {
		t.Fatalf("code = %v, want 3 (leftmost failure)", r.Outcome())
	}
	if r.Program() != "b" || r.Stderr() != "b failed" {
		t.Fatalf("attributed %q/%q, want b/'b failed'", r.Program(), r.Stderr())
	}
	if r.Stdout() != "LAST" {
		t.Fatalf("stdout = %q, want the last stage's output even on failure", r.Stdout())
	}
}

func TestPipefail_SigpipeVictimNotBlamedWhenRealCulpritExists(t *testing.T) {
	r := fold(
		stageOutcome{program: "producer", outcome: signalled(sig(sigPIPE))},
		stageOutcome{program: "consumer", outcome: exited(2), stderr: "real failure"},
	)
	if r.Program() != "consumer" {
		t.Fatalf("blamed %q, want consumer (the non-SIGPIPE culprit)", r.Program())
	}
	if c, ok := r.Code(); !ok || c != 2 {
		t.Fatalf("code = %v, want 2", r.Outcome())
	}
}

func TestPipefail_SigpipeAloneIsBlamed(t *testing.T) {
	r := fold(
		stageOutcome{program: "producer", outcome: signalled(sig(sigPIPE))},
		stageOutcome{program: "consumer", outcome: exited(0)},
	)
	if r.Program() != "producer" {
		t.Fatalf("blamed %q, want producer (the only failure)", r.Program())
	}
	if r.Success() {
		t.Fatal("a lone SIGPIPE producer should not be a success")
	}
}

func TestPipefail_UncheckedInnerStageExempt(t *testing.T) {
	r := fold(
		stageOutcome{program: "producer", outcome: signalled(sig(sigPIPE)), unchecked: true},
		stageOutcome{program: "head", outcome: exited(0)},
	)
	if !r.Success() || r.Program() != "head" {
		t.Fatalf("want success attributed to head, got %q success=%v", r.Program(), r.Success())
	}
}

func TestPipefail_UncheckedProducerDoesNotMaskFailingConsumer(t *testing.T) {
	r := fold(
		stageOutcome{program: "producer", outcome: signalled(sig(sigPIPE)), unchecked: true},
		stageOutcome{program: "consumer", outcome: exited(4), stderr: "still failed"},
	)
	if r.Program() != "consumer" {
		t.Fatalf("blamed %q, want consumer (unchecked producer must not mask it)", r.Program())
	}
}

func TestPipefail_UncheckedLastForgivesNonZeroExit(t *testing.T) {
	r := fold(
		stageOutcome{program: "a", outcome: exited(0)},
		stageOutcome{program: "grep", outcome: exited(1), unchecked: true},
	)
	if !r.Success() {
		t.Fatal("an unchecked last stage's non-zero exit should be a success")
	}
	if c, ok := r.Code(); !ok || c != 1 {
		t.Fatalf("code = %v, want the real code 1 preserved (not fabricated 0)", r.Outcome())
	}
}

func TestPipefail_UncheckedLastDoesNotForgiveTimeout(t *testing.T) {
	r := fold(
		stageOutcome{program: "a", outcome: exited(0)},
		stageOutcome{program: "slow", outcome: timedOut(), unchecked: true},
	)
	if r.Success() {
		t.Fatal("an unchecked last stage's timeout must still surface as failure")
	}
	if !r.Outcome().TimedOut() {
		t.Fatalf("outcome = %v, want timedOut", r.Outcome())
	}
}

func TestPipefail_UncheckedLastDoesNotForgiveSignal(t *testing.T) {
	r := fold(
		stageOutcome{program: "a", outcome: exited(0)},
		stageOutcome{program: "killed", outcome: signalled(sig(9)), unchecked: true},
	)
	if r.Success() {
		t.Fatal("an unchecked last stage's signal kill must still surface as failure")
	}
}

func TestPipefail_CheckedFailureTrumpsUncheckedRegardlessOfOrder(t *testing.T) {
	r := fold(
		stageOutcome{program: "checked", outcome: exited(7), stderr: "boom"},
		stageOutcome{program: "exempt", outcome: exited(9), unchecked: true},
	)
	if r.Program() != "checked" {
		t.Fatalf("blamed %q, want the checked failure", r.Program())
	}
	if c, ok := r.Code(); !ok || c != 7 {
		t.Fatalf("code = %v, want 7", r.Outcome())
	}
}

func TestPipefail_AllUncheckedFailuresReportSuccess(t *testing.T) {
	r := fold(
		stageOutcome{program: "a", outcome: exited(1), unchecked: true},
		stageOutcome{program: "b", outcome: exited(2), unchecked: true},
		stageOutcome{program: "c", outcome: exited(0)},
	)
	if !r.Success() || r.Program() != "c" {
		t.Fatalf("want success attributed to clean last stage c, got %q success=%v", r.Program(), r.Success())
	}
}

func TestPipefail_OkCodesWidenClean(t *testing.T) {
	r := fold(
		stageOutcome{program: "a", outcome: exited(0)},
		stageOutcome{program: "grep", outcome: exited(3), okCodes: []int{3}},
	)
	if !r.Success() {
		t.Fatal("a stage exiting an ok-code should be clean")
	}
}

func TestPipefail_TimedOutStageIsAttributed(t *testing.T) {
	r := fold(
		stageOutcome{program: "slow", outcome: timedOut(), stderr: "deadline"},
		stageOutcome{program: "b", outcome: exited(0)},
	)
	if r.Program() != "slow" || !r.Outcome().TimedOut() {
		t.Fatalf("attributed %q/%v, want slow/timedOut", r.Program(), r.Outcome())
	}
}

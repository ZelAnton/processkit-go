package processkittest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	processkit "github.com/ZelAnton/processkit-go"
)

// seqInner is a fake inner ProcessRunner that returns successive results (repeating
// the last), so a recorder can capture a sequence without a real subprocess.
type seqInner struct {
	results []*processkit.Result
	err     error
	i       int
}

func (s *seqInner) Output(_ context.Context, _ processkit.Invocation) (*processkit.Result, error) {
	if s.err != nil {
		return nil, s.err
	}
	r := s.results[s.i]
	if s.i < len(s.results)-1 {
		s.i++
	}
	return r, nil
}

func result(outcome processkit.Outcome, stdout, stderr string) *processkit.Result {
	return processkit.NewResult(processkit.Invocation{}, outcome, []byte(stdout), []byte(stderr))
}

func TestCassette_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.json")
	ctx := context.Background()

	inner := &seqInner{results: []*processkit.Result{
		result(processkit.Exited(0), "v1.2.3\n", ""),
		result(processkit.Exited(7), "", "boom"),
	}}
	rec := Record(path, inner)
	ok, err := processkit.Command("git", "--version").WithRunner(rec).Output(ctx)
	if err != nil {
		t.Fatalf("record ok run: %v", err)
	}
	bad, err := processkit.Command("git", "explode").WithRunner(rec).Output(ctx)
	if err != nil {
		t.Fatalf("record failing run: %v", err)
	}
	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	rep, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	rok, err := processkit.Command("git", "--version").WithRunner(rep).Output(ctx)
	if err != nil {
		t.Fatalf("replay ok run: %v", err)
	}
	if rok.Stdout() != ok.Stdout() || rok.Outcome().String() != ok.Outcome().String() {
		t.Errorf("ok mismatch: replay %q/%s vs record %q/%s", rok.Stdout(), rok.Outcome(), ok.Stdout(), ok.Outcome())
	}
	rbad, err := processkit.Command("git", "explode").WithRunner(rep).Output(ctx)
	if err != nil {
		t.Fatalf("replay failing run: %v", err)
	}
	if c, _ := rbad.Code(); c != 7 || rbad.Stderr() != bad.Stderr() {
		t.Errorf("bad mismatch: replay code=%d stderr=%q vs record stderr=%q", c, rbad.Stderr(), bad.Stderr())
	}
}

func TestCassette_DuplicateKeyRepeatsLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.json")
	ctx := context.Background()
	inner := &seqInner{results: []*processkit.Result{
		result(processkit.Exited(0), "aaa", ""),
		result(processkit.Exited(0), "bbb", ""),
	}}
	rec := Record(path, inner)
	cmd := processkit.Command("tool", "x").WithRunner(rec)
	_, _ = cmd.Output(ctx)
	_, _ = cmd.Output(ctx)
	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	rep, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	rcmd := processkit.Command("tool", "x").WithRunner(rep)
	want := []string{"aaa", "bbb", "bbb"} // capture order, then the last repeats
	for i, w := range want {
		res, err := rcmd.Output(ctx)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if res.Stdout() != w {
			t.Errorf("replay %d = %q, want %q", i, res.Stdout(), w)
		}
	}
}

func TestCassette_MissIsDistinctFromNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.json")
	ctx := context.Background()
	rec := Record(path, &seqInner{results: []*processkit.Result{result(processkit.Exited(0), "ok", "")}})
	_, _ = processkit.Command("recorded", "cmd").WithRunner(rec).Output(ctx)
	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rep, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	_, err = processkit.Command("never", "recorded").WithRunner(rep).Output(ctx)
	if !errors.Is(err, ErrCassetteMiss) {
		t.Fatalf("err = %v, want ErrCassetteMiss", err)
	}
	if errors.Is(err, processkit.ErrNotFound) {
		t.Error("a cassette miss must NOT read as a missing program (ErrNotFound)")
	}
	var miss *CassetteMissError
	if !errors.As(err, &miss) || miss.Program != "never" {
		t.Errorf("err = %v, want *CassetteMissError{Program:\"never\"}", err)
	}
}

// SECURITY: env values must never reach the cassette file (only names).
func TestCassette_EnvValuesNeverInFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.json")
	ctx := context.Background()
	const secret = "hunter2-very-secret"
	rec := Record(path, &seqInner{results: []*processkit.Result{result(processkit.Exited(0), "ok", "")}})
	_, err := processkit.Command("tool").
		WithEnv("API_TOKEN="+secret, "MODE=fast").
		WithRunner(rec).Output(ctx)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cassette: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "API_TOKEN") || !strings.Contains(body, "MODE") {
		t.Errorf("env names should be recorded:\n%s", body)
	}
	if strings.Contains(body, secret) {
		t.Errorf("env VALUE leaked into the cassette:\n%s", body)
	}
	if strings.Contains(body, "fast") {
		t.Errorf("env VALUE 'fast' leaked into the cassette:\n%s", body)
	}

	// Env is not part of the match key: a replay with different env still matches.
	rep, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if _, err := processkit.Command("tool").WithEnv("MODE=slow").WithRunner(rep).Output(ctx); err != nil {
		t.Errorf("env should not affect matching: %v", err)
	}
}

// SECURITY: the cassette file is written owner-only (0600) on Unix.
func TestCassette_FileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permissions; Windows uses the directory ACL")
	}
	path := filepath.Join(t.TempDir(), "cas.json")
	rec := Record(path, &seqInner{results: []*processkit.Result{result(processkit.Exited(0), "ok", "")}})
	_, _ = processkit.Command("tool").WithRunner(rec).Output(context.Background())
	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("cassette mode = %o, want 0600", perm)
	}
}

func TestCassette_SignalRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.json")
	ctx := context.Background()
	rec := Record(path, &seqInner{results: []*processkit.Result{result(processkit.Signalled(9), "", "")}})
	_, _ = processkit.Command("tool").WithRunner(rec).Output(ctx)
	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rep, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	res, err := processkit.Command("tool").WithRunner(rep).Output(ctx)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if sig, ok := res.Outcome().Signal(); !ok || sig != 9 {
		t.Errorf("Signal = %d (ok=%v), want 9", sig, ok)
	}
}

func TestCassette_ErrorRecordsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.json")
	ctx := context.Background()
	rec := Record(path, &seqInner{err: &processkit.NotFoundError{Program: "tool"}})
	if _, err := processkit.Command("tool").WithRunner(rec).Output(ctx); err == nil {
		t.Fatal("expected the inner error to surface")
	}
	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rep, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	// Nothing was recorded, so any replay is a miss.
	if _, err := processkit.Command("tool").WithRunner(rep).Output(ctx); !errors.Is(err, ErrCassetteMiss) {
		t.Errorf("err = %v, want ErrCassetteMiss (the failed run recorded nothing)", err)
	}
}

func TestCassette_DirIsPartOfKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.json")
	ctx := context.Background()
	rec := Record(path, &seqInner{results: []*processkit.Result{result(processkit.Exited(0), "ok", "")}})
	_, _ = processkit.Command("tool").WithDir("/work/a").WithRunner(rec).Output(ctx)
	if err := rec.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rep, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if _, err := processkit.Command("tool").WithDir("/work/a").WithRunner(rep).Output(ctx); err != nil {
		t.Errorf("same dir should match: %v", err)
	}
	if _, err := processkit.Command("tool").WithDir("/work/b").WithRunner(rep).Output(ctx); !errors.Is(err, ErrCassetteMiss) {
		t.Errorf("different dir should miss; err = %v", err)
	}
}

func TestCassette_SaveThenRecordMore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas.json")
	ctx := context.Background()
	rec := Record(path, &seqInner{results: []*processkit.Result{
		result(processkit.Exited(0), "first", ""),
		result(processkit.Exited(0), "second", ""),
	}})
	_, _ = processkit.Command("a").WithRunner(rec).Output(ctx)
	if err := rec.Save(); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	_, _ = processkit.Command("b").WithRunner(rec).Output(ctx)
	if err := rec.Save(); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	rep, err := Replay(path)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	for _, prog := range []string{"a", "b"} {
		if _, err := processkit.Command(prog).WithRunner(rep).Output(ctx); err != nil {
			t.Errorf("both runs should be on the cassette; %q: %v", prog, err)
		}
	}
}

func TestCassette_LoadErrors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, body, wantSub string
	}{
		{"version", `{"version":99,"entries":[]}`, "version 99"},
		{"malformed", `not json at all`, "valid JSON"},
		{"contradictory", `{"version":1,"entries":[{"program":"t","args":[],"stdout":"","stderr":"","code":0,"signal":9}]}`, "exactly one"},
		{"empty outcome", `{"version":1,"entries":[{"program":"t","args":[],"stdout":"","stderr":"","code":null}]}`, "exactly one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name+".json")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Replay(p)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestCassette_MissingFileIsNotExist(t *testing.T) {
	_, err := Replay(filepath.Join(t.TempDir(), "nope.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want an os.IsNotExist error", err)
	}
}

func TestCassette_EnvNamesSortedDeduped(t *testing.T) {
	// "BARE_SECRET" has no '=', so it is dropped (it could be an unsplit secret); the
	// rest are sorted, deduped, and reduced to names only.
	got := envNames([]string{"Z=1", "A=2", "A=3", "M=", "BARE_SECRET"})
	want := []string{"A", "M", "Z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("envNames = %v, want %v (sorted, deduped, names only, no-'=' dropped)", got, want)
	}
}

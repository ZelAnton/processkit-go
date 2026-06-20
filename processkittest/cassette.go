package processkittest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	processkit "github.com/ZelAnton/processkit-go"
)

// cassetteVersion is the on-disk format version. Bumped only on an incompatible
// schema change; loading an unknown version fails loud rather than misreading.
const cassetteVersion = 1

// maxCassetteBytes caps the size of a cassette accepted on load (64 MiB), so a
// runaway or hostile fixture can't exhaust memory.
const maxCassetteBytes = 64 << 20

// ErrCassetteMiss is returned by a replaying [RecordReplayRunner] when an
// invocation matches no recorded entry. It is deliberately distinct from
// [processkit.ErrNotFound] (a missing *program*): a cassette miss is a missing
// *recording*, and replay never spawns a subprocess to fall back on.
var ErrCassetteMiss = errors.New("processkittest: no cassette entry matches the command")

// CassetteMissError reports that a replayed command matched no recorded entry. It
// carries the full match key (program + args + dir) so a miss is debuggable.
// Matches errors.Is(err, [ErrCassetteMiss]).
type CassetteMissError struct {
	Program string   // the program that was looked up
	Args    []string // its arguments (part of the match key)
	Dir     string   // its working directory (part of the match key)
}

func (e *CassetteMissError) Error() string {
	return fmt.Sprintf("processkittest: no cassette entry for %q (args %v, dir %q) — record one, or check program/args/dir",
		e.Program, e.Args, e.Dir)
}

// Is matches the ErrCassetteMiss sentinel.
func (e *CassetteMissError) Is(target error) bool { return target == ErrCassetteMiss }

// cassette is the whole fixture file: a format version plus the entries in
// capture order.
type cassette struct {
	Version int     `json:"version"`
	Entries []entry `json:"entries"`
}

// entry is one captured Invocation→Result pair.
//
// program, args, dir, stdout, and stderr are stored VERBATIM and can carry secrets
// (a --password=… argv, a token echoed to stdout). Only env *values* are dropped —
// overrides are kept as variable *names* only. The timeout is not stored: it is the
// command's own configuration, re-read at replay time like the live runner.
//
// Strings are UTF-8 text: a cassette is a human-diffable text fixture, so non-UTF-8
// (binary) stdout/stderr is stored lossily — invalid bytes become U+FFFD and do not
// round-trip byte-for-byte. CLI tools emit text, which is unaffected.
type entry struct {
	// --- the match key (program + args + dir; env is excluded) ---
	Program string   `json:"program"`
	Args    []string `json:"args"`
	Dir     string   `json:"dir,omitempty"`
	// --- stored for visibility, NOT matched on ---
	EnvNames []string `json:"env_names,omitempty"` // override names only, sorted+deduped
	// --- the captured output ---
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Code     *int   `json:"code"`                // always emitted (null for a signal/timeout)
	TimedOut bool   `json:"timed_out,omitempty"` //
	Signal   *int   `json:"signal,omitempty"`    // Unix signal number for a signal kill
}

// mode is record (wrap a real runner, capture each run) or replay (serve a loaded
// cassette, never spawn).
type mode int

const (
	modeRecord mode = iota
	modeReplay
)

// RecordReplayRunner is a [processkit.ProcessRunner] that records real
// Invocation→Result pairs to a JSON cassette and replays them hermetically. It
// closes the gap between the hand-written [ScriptedRunner] and the input-asserting
// [RecordingRunner]: run the real tool once in record mode and every result is
// captured to a human-diffable fixture; switch to replay mode and the cassette
// serves the recorded results — fast, hermetic, no subprocess in CI.
//
// Security: a cassette redacts environment *values* (it stores names only), but
// program, args, working directory, stdout, and stderr are stored VERBATIM — any of
// which can carry a secret. Review a fixture before committing it. On Unix the file
// is written owner-only (0600) and the write refuses to follow a symlink; on Windows
// it inherits the directory ACL, so keep the fixture directory restricted.
//
// A cassette is a human-diffable text fixture: stdout/stderr are stored as UTF-8
// text, so binary (non-UTF-8) output is captured lossily and does not round-trip
// byte-for-byte. CLI tools emit text, which is unaffected.
//
// Only runs that complete with a [processkit.Result] are recorded: a spawn failure,
// a not-found program, or a cancellation errors before there is a result to
// capture, so it records nothing (a non-zero exit, signal, or timeout IS a result
// and is recorded). Replay is not timing-faithful — a replayed
// [processkit.Result.Duration] is zero — and, being a custom runner, it emits no
// lifecycle logs (see [processkit.Cmd.WithLogger]).
//
// It is safe for concurrent use. Only the Output verb is supported (the seam the
// capture verbs run through).
type RecordReplayRunner struct {
	mode mode
	path string

	mu    sync.Mutex
	inner processkit.ProcessRunner // record mode: the real runner to wrap
	rec   []entry                  // record mode: captured entries
	slots map[string]*replaySlot   // replay mode: entries grouped by match key
}

// replaySlot holds one key's recorded entries, played in capture order then
// repeating the last forever (so a retry/probe loop past the sequence still gets a
// stable answer).
type replaySlot struct {
	entries []entry
	next    int
}

func (s *replaySlot) play() entry {
	i := s.next
	if i >= len(s.entries) {
		i = len(s.entries) - 1
	} else {
		s.next++
	}
	return s.entries[i]
}

// Record returns a runner that runs every command through inner (e.g. a
// [processkit.JobRunner]) and captures the result, to be written to path by [Save].
// Nothing touches the filesystem until Save. Pass the recorder as a command's
// runner ([processkit.Cmd.WithRunner] or [processkit.CliClient.WithRunner]).
func Record(path string, inner processkit.ProcessRunner) *RecordReplayRunner {
	return &RecordReplayRunner{mode: modeRecord, path: path, inner: inner}
}

// Replay loads the cassette at path and serves its entries hermetically — no
// subprocess is ever spawned. A missing file returns an error that satisfies
// os.IsNotExist; a corrupt file, an oversized file, an unknown format version, or a
// malformed entry is a load error.
func Replay(path string) (*RecordReplayRunner, error) {
	// Reject an oversized file before reading it into memory (a regular file's size
	// is known up front); the post-read check below covers non-regular files.
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxCassetteBytes {
		return nil, fmt.Errorf("processkittest: cassette %q is larger than %d bytes", path, maxCassetteBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // preserves os.IsNotExist for a missing file
	}
	if len(data) > maxCassetteBytes {
		return nil, fmt.Errorf("processkittest: cassette %q is larger than %d bytes", path, maxCassetteBytes)
	}
	var c cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("processkittest: cassette %q is not valid JSON: %w", path, err)
	}
	if c.Version != cassetteVersion {
		return nil, fmt.Errorf("processkittest: cassette %q version %d is not supported (this build reads version %d)",
			path, c.Version, cassetteVersion)
	}
	slots := make(map[string]*replaySlot)
	for i := range c.Entries {
		e := c.Entries[i]
		if err := validateOutcome(e); err != nil {
			return nil, fmt.Errorf("processkittest: cassette %q entry %d: %w", path, i, err)
		}
		k := keyOf(e.Program, e.Args, e.Dir)
		s := slots[k]
		if s == nil {
			s = &replaySlot{}
			slots[k] = s
		}
		s.entries = append(s.entries, e)
	}
	return &RecordReplayRunner{mode: modeReplay, path: path, slots: slots}, nil
}

// Output implements [processkit.ProcessRunner].
func (r *RecordReplayRunner) Output(ctx context.Context, inv processkit.Invocation) (*processkit.Result, error) {
	if r.mode == modeReplay {
		return r.replay(inv)
	}
	return r.recordOne(ctx, inv)
}

func (r *RecordReplayRunner) recordOne(ctx context.Context, inv processkit.Invocation) (*processkit.Result, error) {
	res, err := r.inner.Output(ctx, inv)
	if err != nil {
		return nil, err // errors (spawn failure, cancel) record nothing
	}
	e := entryOf(inv, res)
	r.mu.Lock()
	r.rec = append(r.rec, e)
	r.mu.Unlock()
	return res, nil
}

func (r *RecordReplayRunner) replay(inv processkit.Invocation) (*processkit.Result, error) {
	r.mu.Lock()
	s := r.slots[keyOf(inv.Program, inv.Args, inv.Dir)]
	if s == nil {
		r.mu.Unlock()
		return nil, &CassetteMissError{Program: inv.Program, Args: append([]string(nil), inv.Args...), Dir: inv.Dir}
	}
	e := s.play()
	r.mu.Unlock()

	outcome, err := outcomeOf(e)
	if err != nil {
		return nil, fmt.Errorf("processkittest: cassette %q: %w", r.path, err)
	}
	return processkit.NewResult(inv, outcome, []byte(e.Stdout), []byte(e.Stderr)), nil
}

// Save writes the cassette now (record mode). Idempotent — it rewrites the full
// file each time, so runs recorded after a Save are still covered by a later Save.
// In replay mode it is a no-op. Unlike the Rust crate there is no automatic
// drop-time flush (Go has no destructor): call Save explicitly after a successful
// recording session. Because it persists secret-bearing fields, prefer not to defer
// it across a code path that may panic mid-recording.
func (r *RecordReplayRunner) Save() error {
	if r.mode == modeReplay {
		return nil
	}
	r.mu.Lock()
	c := cassette{Version: cassetteVersion, Entries: append([]entry(nil), r.rec...)}
	r.mu.Unlock()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSecure(r.path, data)
}

// entryOf captures inv+res into an entry, dropping env values (keeping names) and
// the duration.
func entryOf(inv processkit.Invocation, res *processkit.Result) entry {
	e := entry{
		Program:  inv.Program,
		Args:     append([]string{}, inv.Args...), // non-nil → "[]" not "null", and a stable copy
		Dir:      inv.Dir,
		EnvNames: envNames(inv.Env),
		Stdout:   string(res.StdoutBytes()),
		Stderr:   res.Stderr(),
	}
	oc := res.Outcome()
	switch {
	case oc.TimedOut():
		e.TimedOut = true
	default:
		if c, ok := oc.Code(); ok {
			e.Code = &c
		} else if s, ok := oc.Signal(); ok {
			e.Signal = &s
		}
		// else: a signal kill with no known number — not representable as a
		// recordable outcome (Go can't rebuild Signalled-without-signal); it would
		// fail validateOutcome on load. Real recordings always carry the signal.
	}
	return e
}

// outcomeOf rebuilds an Outcome from an entry. Exactly one of code / timed_out /
// signal must be set (validated on load).
func outcomeOf(e entry) (processkit.Outcome, error) {
	if err := validateOutcome(e); err != nil {
		return processkit.Outcome{}, err
	}
	switch {
	case e.TimedOut:
		return processkit.TimedOut(), nil
	case e.Code != nil:
		return processkit.Exited(*e.Code), nil
	default:
		return processkit.Signalled(*e.Signal), nil
	}
}

// validateOutcome enforces that exactly one of code / timed_out / signal is set.
// (The Rust crate also accepts "none set" as Signalled(unknown), but Go's public
// API can't build that outcome, so a recorded signal kill always carries its
// number and an all-empty outcome is rejected as malformed.)
func validateOutcome(e entry) error {
	n := 0
	if e.Code != nil {
		n++
	}
	if e.TimedOut {
		n++
	}
	if e.Signal != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("entry must set exactly one of code / timed_out / signal (got %d)", n)
	}
	return nil
}

// envNames extracts the override variable names (the part before the first '='),
// sorted and deduplicated. Values are deliberately discarded — they routinely carry
// secrets and a cassette must never persist them.
func envNames(env []string) []string {
	if len(env) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(env))
	names := make([]string, 0, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			// Not "KEY=VALUE": no name/value boundary, so it could be an unsplit
			// secret. Don't persist it — a cassette stores names, never values.
			continue
		}
		name := kv[:i]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// keyOf is the replay match key: program + args + dir, canonicalised via JSON so
// any bytes compare safely and a nil-vs-empty args difference can't split a match.
// Env is excluded so an irrelevant env difference between record and replay can't
// cause a spurious miss.
func keyOf(program string, args []string, dir string) string {
	b, _ := json.Marshal([]any{program, append([]string{}, args...), dir})
	return string(b)
}

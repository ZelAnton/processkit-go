package processkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ZelAnton/processkit-go/internal/sys"
)

// Pipeline is a shell-free chain of commands wired stdout→stdin, like a | b | c.
// Every stage runs inside one shared kill-on-drop container, so the whole chain
// lives and dies together. Build it with [Pipe] and finish with a verb
// ([Pipeline.Output], [Pipeline.Run], [Pipeline.ExitCode], [Pipeline.Probe]).
//
// The connection between stages is a real OS pipe (no shell, no temp files): each
// stage's standard output is the next stage's standard input. Only the LAST
// stage's stdout is captured; every stage's stderr is captured for failure
// attribution. The first stage's stdin can be fed with [Pipeline.WithStdin].
//
// Like [Cmd], a Pipeline is a value built by chainable WithX methods (each
// returning a new, independent *Pipeline) and is safe to reuse and re-run.
type Pipeline struct {
	stages  []*Cmd
	timeout time.Duration
	stdin   io.Reader
	log     runLog
}

// Pipe builds a pipeline from two or more stages, wiring each stage's stdout to
// the next stage's stdin. A pipeline needs at least two stages; the verbs return
// [ErrTooFewStages] for fewer. Each stage's program, arguments, [Cmd.WithDir],
// [Cmd.WithEnv], [Cmd.WithTimeout] (per-stage deadline), [Cmd.WithOkCodes], and
// [Cmd.WithUncheckedInPipe] are honoured; its [Cmd.WithRunner], [Cmd.WithRetry], and
// [Cmd.WithStdin] are not (a pipeline always runs real processes and wires each
// stage's stdin from the previous stage — feed the chain's head with
// [Pipeline.WithStdin] instead; retry the whole chain).
func Pipe(stages ...*Cmd) *Pipeline {
	return &Pipeline{stages: append([]*Cmd(nil), stages...)}
}

func (p *Pipeline) clone() *Pipeline {
	cp := *p
	cp.stages = append([]*Cmd(nil), p.stages...)
	return &cp
}

// WithTimeout returns a copy of the pipeline bounded by a whole-chain deadline.
// At the deadline the entire chain is killed and the [Result] reports
// [Outcome.TimedOut] with no captured stdout (the chain was abandoned) and the
// joined chain name as the program. This is distinct from a stage's own
// [Cmd.WithTimeout], which kills only that stage and is attributed to it.
func (p *Pipeline) WithTimeout(d time.Duration) *Pipeline {
	cp := p.clone()
	cp.timeout = d
	return cp
}

// WithStdin returns a copy of the pipeline whose first stage reads its standard
// input from r. Inner stages always read the previous stage's output, so r feeds
// only the head of the chain. r is consumed as the chain runs; to re-run a
// pipeline, supply a re-readable source (e.g. a fresh [strings.Reader] each time).
func (p *Pipeline) WithStdin(r io.Reader) *Pipeline {
	cp := p.clone()
	cp.stdin = r
	return cp
}

// WithLogger returns a copy of the pipeline that emits structured [log/slog]
// events when the chain starts and finishes (the finish event carries the
// attributed outcome and elapsed time). The default is no logging; pass nil to
// disable. As always, the stages' arguments and environment are never logged.
func (p *Pipeline) WithLogger(logger *slog.Logger) *Pipeline {
	cp := p.clone()
	cp.log = runLog{logger}
	return cp
}

// name renders the chain as "a | b | c" for diagnostics.
func (p *Pipeline) name() string {
	names := make([]string, len(p.stages))
	for i, s := range p.stages {
		names[i] = s.program
	}
	return strings.Join(names, " | ")
}

// Output runs the whole chain and returns a single [Result] folded by pipefail
// attribution: stdout is the last stage's, while the program / stderr / exit
// outcome are the first failing (non-exempt) stage's — see the package overview.
// A non-zero exit anywhere is data in the Result, not an error; only a spawn
// failure, a cancelled context, the caller's context deadline, or fewer than two
// stages ([ErrTooFewStages]) errors.
//
// Because a chain has no single program, [Result.Program] and [Result.Args] on a
// pipeline Result reflect the *attributed* stage (the blamed one, or the last on
// success) — except a whole-chain timeout, whose Program is the joined "a | b | c"
// chain name. Stdout is always the last stage's.
func (p *Pipeline) Output(ctx context.Context) (*Result, error) {
	if len(p.stages) < 2 {
		return nil, ErrTooFewStages
	}

	parent := ctx
	runCtx := ctx
	if p.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(parent, p.timeout)
		defer cancel()
	}
	if parent.Err() != nil { // already cancelled / expired before we spawn anything
		return nil, &CancelError{Program: p.name(), Cause: parent.Err()}
	}

	job, err := sys.NewJob(sys.Limits{}) // a pipeline shares one container, no resource caps
	if err != nil {
		return nil, &StartError{Program: p.name(), Err: err}
	}
	defer job.Close()

	n := len(p.stages)
	ecmds := make([]*exec.Cmd, n)
	stageCtxs := make([]context.Context, n)
	stderrs := make([]bytes.Buffer, n)
	var lastStdout bytes.Buffer

	var carryStdin *os.File // read end feeding the next stage's stdin
	var parentEnds []*os.File
	started := make([]*exec.Cmd, 0, n)
	fail := func(err error) (*Result, error) {
		_ = job.Kill()
		// Close the parent's pipe ends BEFORE waiting: a child blocked writing to a
		// pipe whose reader never started then gets EPIPE, so its Wait can't hang on
		// a parent-held end (the success path likewise closes ends before waiting).
		for _, f := range parentEnds {
			_ = f.Close()
		}
		for _, c := range started {
			_ = c.Wait()
		}
		// The caller's context ending the run wins: a Start/Assign that failed
		// because the caller cancelled is a cancellation, not a spawn failure.
		if parent.Err() != nil {
			return nil, &CancelError{Program: p.name(), Cause: parent.Err()}
		}
		return nil, err
	}

	start := time.Now()
	for i := 0; i < n; i++ {
		stage := p.stages[i]
		stageCtx := runCtx
		if stage.timeout > 0 {
			var cancel context.CancelFunc
			stageCtx, cancel = context.WithTimeout(runCtx, stage.timeout)
			defer cancel()
		}
		stageCtxs[i] = stageCtx

		inv := stage.invocation()
		ecmd := exec.CommandContext(stageCtx, inv.Program, inv.Args...)
		ecmd.Dir = inv.Dir
		ecmd.Env = inv.Env
		ecmd.WaitDelay = waitDelay
		ecmd.Stderr = &stderrs[i]
		if i == 0 {
			ecmd.Stdin = p.stdin
		} else {
			ecmd.Stdin = carryStdin
		}
		if i == n-1 {
			ecmd.Stdout = &lastStdout
		} else {
			pr, pw, perr := os.Pipe()
			if perr != nil {
				return fail(&StartError{Program: inv.Program, Err: perr})
			}
			ecmd.Stdout = pw
			parentEnds = append(parentEnds, pr, pw)
			carryStdin = pr
		}

		if err := job.Configure(ecmd); err != nil {
			return fail(&StartError{Program: inv.Program, Err: err})
		}
		if err := ecmd.Start(); err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return fail(&NotFoundError{Program: inv.Program, Searched: searchedPath(inv.Program)})
			}
			return fail(&StartError{Program: inv.Program, Err: err})
		}
		started = append(started, ecmd)
		if err := job.Assign(ecmd); err != nil {
			return fail(&StartError{Program: inv.Program, Err: err})
		}
		ecmds[i] = ecmd
	}
	p.log.pipelineStarted(p.name(), n)

	// Close the parent's copies of the inter-stage pipe ends so that, when a stage
	// exits, the next stage sees EOF on its stdin (the children hold their own dups).
	for _, f := range parentEnds {
		_ = f.Close()
	}

	// Wait every stage concurrently so a stderr-chatty inner stage can't wedge the
	// reap behind a stage that is still draining.
	var wg sync.WaitGroup
	for i := range ecmds {
		wg.Add(1)
		go func(c *exec.Cmd) {
			defer wg.Done()
			_ = c.Wait()
		}(ecmds[i])
	}
	wg.Wait()
	duration := time.Since(start)
	_ = job.Kill() // reap any grandchildren that outlived the chain

	// The caller's context ending the run wins over everything: no captured Result.
	if parent.Err() != nil {
		return nil, &CancelError{Program: p.name(), Cause: parent.Err()}
	}
	// The pipeline's own deadline: a whole-chain timeout. The Result is deliberately
	// skeletal — no partial stdout (the chain was abandoned), no stderr or ok-codes,
	// and the joined chain name as the program. A timeout is never a success anyway
	// (timedOut has no Code), so the empty ok-codes change nothing.
	if p.timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		p.log.pipelineFinished(p.name(), timedOut(), duration)
		return &Result{
			program:   p.name(),
			outcome:   timedOut(),
			duration:  duration,
			mechanism: toMechanism(job.Mechanism()),
		}, nil
	}

	stages := make([]stageOutcome, n)
	for i, stage := range p.stages {
		var oc Outcome
		switch {
		case stage.timeout > 0 && errors.Is(stageCtxs[i].Err(), context.DeadlineExceeded):
			oc = timedOut() // this stage's own deadline fired
		case ecmds[i].ProcessState != nil:
			oc = outcomeOf(ecmds[i].ProcessState)
		default:
			oc = exited(0)
		}
		stages[i] = stageOutcome{
			program:   stage.program,
			args:      append([]string(nil), stage.args...),
			outcome:   oc,
			stderr:    normalizeNewlines(stderrs[i].Bytes()),
			okCodes:   append([]int(nil), stage.okCodes...),
			unchecked: stage.uncheckedInPipe,
		}
	}
	res := pipefail(stages, lastStdout.Bytes(), toMechanism(job.Mechanism()), duration)
	p.log.pipelineFinished(res.program, res.outcome, duration)
	return res, nil
}

// Run requires the chain to succeed and returns the last stage's stdout as text
// with trailing whitespace trimmed. A pipefail-attributed failure is an error.
func (p *Pipeline) Run(ctx context.Context) (string, error) {
	res, err := p.Output(ctx)
	if err != nil {
		return "", err
	}
	if err := res.Err(); err != nil {
		return "", err
	}
	return strings.TrimRight(res.Stdout(), " \t\r\n"), nil
}

// ExitCode runs the chain and returns the pipefail-attributed exit code. A chain
// with no exit code (a timeout or signal kill) is an error.
func (p *Pipeline) ExitCode(ctx context.Context) (int, error) {
	res, err := p.Output(ctx)
	if err != nil {
		return 0, err
	}
	code, ok := res.Code()
	if !ok {
		return 0, res.toExitError()
	}
	return code, nil
}

// Probe runs the chain as a yes/no predicate on the pipefail-attributed code:
// exit 0 → true, exit 1 → false, anything else → error. OkCodes does not apply.
func (p *Pipeline) Probe(ctx context.Context) (bool, error) {
	res, err := p.Output(ctx)
	if err != nil {
		return false, err
	}
	code, ok := res.Code()
	if !ok {
		return false, res.toExitError()
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, res.toExitError()
	}
}

// stageOutcome is one stage's contribution to the pipefail fold.
type stageOutcome struct {
	program   string
	args      []string
	outcome   Outcome
	stderr    string
	okCodes   []int
	unchecked bool
}

// clean reports whether the stage exited with a success code (0 or an ok-code).
// A signal kill or a timeout is never clean.
func (s stageOutcome) clean() bool {
	code, ok := s.outcome.Code()
	if !ok {
		return false
	}
	if code == 0 {
		return true
	}
	for _, c := range s.okCodes {
		if code == c {
			return true
		}
	}
	return false
}

// sigPIPE is the Unix signal delivered to a writer whose reader has closed. A
// pipeline producer killed by it is a victim of a downstream stage, not a culprit.
const sigPIPE = 13

// isSigpipe reports whether the outcome is a SIGPIPE kill (Unix only — on Windows
// a kill is never reported as a signal, so this is always false there).
func isSigpipe(o Outcome) bool {
	sig, ok := o.Signal()
	return ok && sig == sigPIPE
}

// pipefail folds the stage outcomes into one [Result] with bash `set -o pipefail`
// semantics and leftmost-wins attribution: the program / stderr / outcome / ok-codes
// come from the FIRST unclean, non-exempt stage (preferring a real culprit over an
// upstream SIGPIPE victim), while stdout is ALWAYS the last stage's. If no checked
// stage failed, the last stage speaks; an exempt last stage's non-zero exit is
// forgiven by widening its ok-codes (never by fabricating a 0).
func pipefail(stages []stageOutcome, lastStdout []byte, mechanism Mechanism, duration time.Duration) *Result {
	last := stages[len(stages)-1]

	var failures []stageOutcome
	for _, s := range stages {
		if !s.unchecked && !s.clean() {
			failures = append(failures, s)
		}
	}
	if len(failures) > 0 {
		blamed := failures[0]
		for _, s := range failures {
			if !isSigpipe(s.outcome) {
				blamed = s
				break
			}
		}
		return &Result{
			program:   blamed.program,
			args:      blamed.args,
			outcome:   blamed.outcome,
			stdout:    lastStdout,
			stderr:    blamed.stderr,
			okCodes:   blamed.okCodes,
			duration:  duration,
			mechanism: mechanism,
		}
	}

	// No checked failure: the last stage speaks.
	okCodes := last.okCodes
	if last.unchecked {
		if code, ok := last.outcome.Code(); ok && !last.clean() {
			okCodes = append(append([]int(nil), last.okCodes...), code)
		}
	}
	return &Result{
		program:   last.program,
		args:      last.args,
		outcome:   last.outcome,
		stdout:    lastStdout,
		stderr:    last.stderr,
		okCodes:   okCodes,
		duration:  duration,
		mechanism: mechanism,
	}
}

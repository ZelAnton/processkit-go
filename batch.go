package processkit

import (
	"context"
	"errors"
	"reflect"
	"sync"
)

// WaitAny waits for whichever of the given processes exits first, returning its
// index, its [Outcome], and any wait error. It returns early with ctx's error if
// the context is done first. The processes are only observed — the losers stay
// usable afterwards.
func WaitAny(ctx context.Context, procs ...*RunningProcess) (int, Outcome, error) {
	if len(procs) == 0 {
		return 0, Outcome{}, errors.New("processkit: WaitAny needs at least one process")
	}
	cases := make([]reflect.SelectCase, 0, len(procs)+1)
	for _, p := range procs {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(p.done)})
	}
	cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})

	chosen, _, _ := reflect.Select(cases)
	if chosen == len(procs) { // the ctx.Done() case
		return 0, Outcome{}, ctx.Err()
	}
	return chosen, procs[chosen].outcome, procs[chosen].waitErr
}

// WaitAll waits for every process to exit, returning their [Outcome]s in input
// order. It returns early with ctx's error if the context is done first, or with
// the first process's wait error — in which case the returned slice is nil (a
// wait error is a rare reap failure, not a non-zero exit, which is carried in the
// [Outcome]).
func WaitAll(ctx context.Context, procs ...*RunningProcess) ([]Outcome, error) {
	outcomes := make([]Outcome, len(procs))
	for i, p := range procs {
		select {
		case <-p.done:
			if p.waitErr != nil {
				return nil, p.waitErr
			}
			outcomes[i] = p.outcome
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return outcomes, nil
}

// BatchOutput is one command's independent result from [OutputAll]: either a
// captured Result (any exit code) or an error (spawn failure, cancellation).
type BatchOutput struct {
	Result *Result
	Err    error
}

// OutputAll runs every command to completion and captures each result, with at
// most concurrency runs in flight at once (so fanning out hundreds of commands
// can't exhaust file descriptors or the process table). It is collect-all: a
// non-zero exit never short-circuits the batch — each element is independent, in
// input order.
func OutputAll(ctx context.Context, cmds []*Cmd, concurrency int) []BatchOutput {
	if concurrency < 1 {
		concurrency = 1
	}
	out := make([]BatchOutput, len(cmds))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, c := range cmds {
		wg.Add(1)
		go func(i int, c *Cmd) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := c.Output(ctx)
			out[i] = BatchOutput{Result: res, Err: err}
		}(i, c)
	}
	wg.Wait()
	return out
}

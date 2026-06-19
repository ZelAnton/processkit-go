//go:build unix

package processkit

import (
	"os"
	"syscall"
)

// outcomeOf maps a finished process state to an Outcome. A process killed by a
// signal reports Signalled with the signal number; otherwise its exit code.
// (Timeout and cancellation are decided by the caller from the context, not here.)
func outcomeOf(ps *os.ProcessState) Outcome {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sig := int(ws.Signal())
		return signalled(&sig)
	}
	return exited(ps.ExitCode())
}

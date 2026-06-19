//go:build windows

package processkit

import "os"

// outcomeOf maps a finished process state to an Outcome. Windows has no signal
// abstraction — a killed process reports an exit code (e.g. 1 from
// TerminateProcess), never Signalled. So Outcome.Signalled is Unix-only.
func outcomeOf(ps *os.ProcessState) Outcome {
	return exited(ps.ExitCode())
}

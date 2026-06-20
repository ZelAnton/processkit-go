//go:build windows || unix

// Package sys is the platform layer behind processkit's whole-tree, no-orphan
// containment. A Job owns one or more started processes and everything they
// spawn, so the whole group can be killed as a unit. One implementation is
// compiled per target, all satisfying the same Job interface.
package sys

import (
	"errors"
	"os/exec"
)

// Mechanism reports which OS primitive a Job uses (mapped to the public
// processkit.Mechanism by the caller, which can't import this internal package).
type Mechanism int

const (
	Unknown Mechanism = iota
	JobObject
	CgroupV2
	ProcessGroup
)

// ErrUnsupported is returned by an operation a platform can't perform (e.g. a
// non-kill Signal on Windows, whose Job Object only supports terminate).
var ErrUnsupported = errors.New("sys: operation not supported on this platform")

// Job contains a set of started process trees. A Job may hold one child (a
// private per-run job) or many (a shared group). Per child:
//
//	j.Configure(cmd)   // set SysProcAttr for containment, before Start
//	cmd.Start()
//	j.Assign(cmd)      // contain the started child (kills just it on failure)
//
// Then the whole group is torn down with Kill (or, gracefully, Signal + Kill)
// and Close.
type Job interface {
	// Configure prepares cmd for containment before it is started. It may create
	// OS resources (e.g. a cgroup) and fail — the caller must not Start cmd if it
	// returns an error.
	Configure(cmd *exec.Cmd) error
	// Assign contains a just-started child. May be called many times (a shared
	// group). On any failure it leaves no uncontained survivor (terminating just
	// that child if needed) and returns the error.
	Assign(cmd *exec.Cmd) error
	// Signal broadcasts sig (a signal number) to every member. Returns
	// ErrUnsupported where a platform can't deliver it (Windows supports only the
	// terminate path — use Kill).
	Signal(sig int) error
	// Kill hard-kills every member. Idempotent; a group that already exited is
	// success.
	Kill() error
	// Close releases any OS handles held by the job. On Windows, if Kill was not
	// called first, closing the last job handle itself reaps the tree
	// (KILL_ON_JOB_CLOSE) — so call Kill before Close.
	Close() error
	// Mechanism reports the containment actually in effect.
	Mechanism() Mechanism
}

// NewJob creates a fresh, empty job for the current platform.
func NewJob() (Job, error) { return newJob() }

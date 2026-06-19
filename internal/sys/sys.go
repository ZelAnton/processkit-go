//go:build windows || unix

// Package sys is the platform layer behind processkit's whole-tree, no-orphan
// containment. A Job owns a started process and everything it spawns, so the
// whole tree can be killed as a unit. One implementation is compiled per target,
// all satisfying the same Job interface.
package sys

import "os/exec"

// Mechanism reports which OS primitive a Job uses (mapped to the public
// processkit.Mechanism by the caller, which can't import this internal package).
type Mechanism int

const (
	Unknown Mechanism = iota
	JobObject
	CgroupV2
	ProcessGroup
)

// Job contains a started process tree. Lifecycle:
//
//	j := NewJob()
//	j.Configure(cmd)   // set SysProcAttr for containment, before Start
//	cmd.Start()
//	j.Assign(cmd)      // contain the started child (kills it on failure — no orphan)
//	... run, Wait ...
//	j.Kill()           // reap the whole tree (grandchildren included); idempotent
//	j.Close()          // release OS handles
type Job interface {
	// Configure prepares cmd for containment before it is started. It may create
	// OS resources (e.g. a cgroup) and fail — the caller must not Start cmd if it
	// returns an error. (The Job Object and process-group backends never fail here;
	// a future cgroup backend can.)
	Configure(cmd *exec.Cmd) error
	// Assign contains the just-started child. On any failure it leaves no
	// uncontained survivor (terminating the child if needed) and returns the error.
	Assign(cmd *exec.Cmd) error
	// Kill hard-kills every process in the job. Idempotent; a tree that already
	// exited is success.
	Kill() error
	// Close releases any OS handles held by the job. On Windows, if Kill was not
	// called first, closing the last job handle itself reaps the tree
	// (KILL_ON_JOB_CLOSE) — so call Kill before Close.
	Close() error
	// Mechanism reports the containment actually in effect.
	Mechanism() Mechanism
}

// NewJob creates a fresh, empty job for the current platform.
func NewJob() Job { return newJob() }

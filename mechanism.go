package processkit

// Mechanism reports which operating-system primitive contains a process tree —
// the basis of processkit's whole-tree, no-orphan teardown guarantee. It is
// observable so callers can tell *how* containment is enforced, in particular
// when it is the weaker POSIX process group rather than a cgroup or Job Object.
type Mechanism int

const (
	// MechanismUnknown is the zero value: no containment has been determined yet.
	MechanismUnknown Mechanism = iota

	// MechanismJobObject is a Windows Job Object with
	// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: closing or terminating the job reaps
	// every process in the tree, grandchildren included.
	MechanismJobObject

	// MechanismProcessGroup is a POSIX process group, torn down via killpg — the
	// mechanism on every Unix (Linux, macOS, the BSDs) today. Weaker than a Job
	// Object: a child that calls setsid escapes it.
	MechanismProcessGroup

	// MechanismCgroupV2 is a Linux cgroup v2 subtree: children are placed atomically
	// at clone (clone3 CLONE_INTO_CGROUP, kernel ≥ 5.7) and the tree is torn down
	// with cgroup.kill. It is preferred on Linux wherever a cgroup can be made, and
	// additionally enforces [WithMemoryMax] / [WithMaxProcesses] / [WithCPUQuota]
	// where the controllers are delegated (the real cgroup-v2 root). Linux degrades
	// to MechanismProcessGroup where no cgroup is available.
	MechanismCgroupV2
)

// String returns the mechanism's name (e.g. "JobObject").
func (m Mechanism) String() string {
	switch m {
	case MechanismJobObject:
		return "JobObject"
	case MechanismProcessGroup:
		return "ProcessGroup"
	case MechanismCgroupV2:
		return "CgroupV2"
	default:
		return "Unknown"
	}
}

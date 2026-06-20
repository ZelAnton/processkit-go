//go:build unix && !linux

package sys

import "errors"

// newJob builds the containment backend for macOS and the BSDs: a POSIX process
// group. These platforms have no whole-tree resource-limit primitive, so a
// requested cap fails fast rather than handing back an unbounded tree the caller
// believes is capped. (Linux has its own newJob in job_linux.go that prefers a
// cgroup.)
func newJob(limits Limits) (Job, error) {
	if limits.Any() {
		return nil, errors.New("sys: resource limits require a cgroup or Job Object; unavailable on this target")
	}
	return newPgroupJob(), nil
}

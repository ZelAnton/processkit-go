//go:build linux

package sys

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// newJob prefers a cgroup v2 subtree; it degrades to a POSIX process group when no
// cgroup can be made (no cgroup v2, no delegation, a read-only fs, or a
// container/systemd scope where the "no internal processes" rule forbids it). The
// choice is observable via Mechanism — never silent. A process group can't enforce a
// resource cap, so if any limit was requested and the cgroup couldn't be made, this
// fails fast rather than hand back an uncapped tree the caller believes is capped.
func newJob(limits Limits) (Job, error) {
	cg, err := createCgroup(limits)
	if err != nil {
		if limits.Any() {
			return nil, err
		}
		return newPgroupJob(), nil
	}
	return cg, nil
}

// cgroupJob contains a process tree in a cgroup v2 subtree. A child is placed in the
// cgroup ATOMICALLY at clone, via clone3's CLONE_INTO_CGROUP (Go's
// SysProcAttr.UseCgroupFD, kernel ≥ 5.7) — so it is born inside the cgroup and its
// forks inherit membership with no escape window (unlike the process group's
// post-fork tracking, this closes the grandchild race entirely). Teardown is the
// atomic cgroup.kill (kernel ≥ 5.14, with a freeze + SIGKILL-sweep fallback) —
// race-free, unlike a pid-based killpg.
type cgroupJob struct {
	path  string   // the cgroup directory under /sys/fs/cgroup
	dir   *os.File // an open handle to that directory, kept open to keep dirFD valid
	dirFD int      // dir.Fd() captured once, for UseCgroupFD atomic placement
}

func (j *cgroupJob) Configure(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Clone the child straight into the cgroup (atomic; its forks inherit it).
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = j.dirFD
	return nil
}

func (j *cgroupJob) Assign(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("sys: Assign called before the process started")
	}
	// The child was placed into the cgroup atomically at clone (Configure set
	// UseCgroupFD), so there is nothing to do here — a successful Start means it (and
	// its future descendants) are already contained.
	return nil
}

// contain moves pid into the cgroup. A pid that has already exited (ESRCH) is a
// benign success — there is nothing left to contain.
func (j *cgroupJob) contain(pid int) error {
	if err := writeCgroup(filepath.Join(j.path, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("sys: placing pid %d into the cgroup: %w", pid, err)
	}
	return nil
}

func (j *cgroupJob) Signal(sig int) error {
	// cgroup v2 has no whole-subtree arbitrary-signal primitive (only kill/freeze),
	// so deliver per pid to the current members. ESRCH/EPERM are benign.
	var firstErr error
	for _, pid := range j.members() {
		deliver(pid, sig, &firstErr)
	}
	return firstErr
}

func (j *cgroupJob) Suspend() error { return j.freeze(true) }
func (j *cgroupJob) Resume() error  { return j.freeze(false) }

// freeze writes cgroup.freeze (kernel ≥ 5.2): "1" freezes the whole subtree, "0"
// thaws it. Only the file being absent (older kernel) falls back to a per-pid
// SIGSTOP/SIGCONT sweep; any other error on a file that exists is surfaced.
func (j *cgroupJob) freeze(frozen bool) error {
	val := "0"
	if frozen {
		val = "1"
	}
	err := writeCgroup(filepath.Join(j.path, "cgroup.freeze"), val)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sig := syscall.SIGCONT
	if frozen {
		sig = syscall.SIGSTOP
	}
	return j.Signal(int(sig))
}

func (j *cgroupJob) Adopt(pid int) error { return j.contain(pid) }

// Kill hard-kills the whole subtree. cgroup.kill (kernel ≥ 5.14) does it atomically;
// on any failure it falls back to freezing the subtree (so a fork bomb can't
// out-spawn the sweep) then a bounded per-pid SIGKILL sweep, then a thaw.
func (j *cgroupJob) Kill() error {
	if writeCgroup(filepath.Join(j.path, "cgroup.kill"), "1") == nil {
		return nil
	}
	_ = writeCgroup(filepath.Join(j.path, "cgroup.freeze"), "1")
	for i := 0; i < 50; i++ {
		members := j.members()
		if len(members) == 0 {
			break
		}
		for _, pid := range members {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(2 * time.Millisecond)
	}
	_ = writeCgroup(filepath.Join(j.path, "cgroup.freeze"), "0")
	if len(j.members()) == 0 {
		return nil
	}
	return errors.New("sys: cgroup did not drain after the bounded SIGKILL sweep (kernel < 5.14 fallback)")
}

// Close drains the (async cgroup.kill) subtree, then removes the cgroup directory.
// rmdir returns EBUSY until every member has left cgroup.procs (a process leaves on
// exit, before it is reaped), so this drains within milliseconds; it is safe from a
// defer, not under a hot lock.
func (j *cgroupJob) Close() error {
	for i := 0; i < 50; i++ {
		if len(j.members()) == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if j.dir != nil {
		_ = j.dir.Close()
	}
	_ = os.Remove(j.path) // best-effort; a non-empty dir (survivors) is intentionally kept
	return nil
}

func (j *cgroupJob) Mechanism() Mechanism { return CgroupV2 }

// Stats reports the live member count and sums each member's /proc CPU and peak
// memory (the cgroup has no controllers enabled, so cpu/memory aren't read from it).
func (j *cgroupJob) Stats() (Stats, error) {
	pids := j.members()
	s := Stats{ActiveProcesses: len(pids)}
	for _, pid := range pids {
		m := processMetrics(pid)
		if m.HasCPU {
			s.CPUTime = addDurationSat(s.CPUTime, m.CPUTime)
			s.HasCPU = true
		}
		if m.HasMem {
			s.PeakMemoryBytes = addU64Sat(s.PeakMemoryBytes, m.PeakMemory)
			s.HasMem = true
		}
	}
	return s, nil
}

// members reads the live pids in the cgroup (empty if the file is gone). A 0 or
// negative line is never a real member, so it is filtered out.
func (j *cgroupJob) members() []int {
	data, err := os.ReadFile(filepath.Join(j.path, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(data), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// --- cgroup creation & limits ---

var (
	cgroupSalt     string
	cgroupSaltOnce sync.Once
	cgroupCounter  atomic.Uint64
)

// cgroupNameSalt is a per-process hex salt (wall-clock nanos, once), so a leftover
// directory from a crashed prior run with a recycled pid can't collide with — and
// silently downgrade — a fresh run (a cgroup, unlike a Windows Job Object, is not
// reaped by the kernel when its creator dies).
func cgroupNameSalt() string {
	cgroupSaltOnce.Do(func() { cgroupSalt = strconv.FormatInt(time.Now().UnixNano(), 16) })
	return cgroupSalt
}

var (
	cgroupFDOK   bool
	cgroupFDOnce sync.Once
)

// cgroupFDSupported reports whether the kernel is ≥ 5.7 — the floor for clone3
// CLONE_INTO_CGROUP (Go's SysProcAttr.UseCgroupFD). Checked once.
func cgroupFDSupported() bool {
	cgroupFDOnce.Do(func() {
		var u syscall.Utsname
		if syscall.Uname(&u) == nil {
			cgroupFDOK = kernelAtLeast(unameRelease(&u), 5, 7)
		}
	})
	return cgroupFDOK
}

// unameRelease extracts the NUL-terminated release string from a Utsname (its
// Release field is a fixed byte array whose element type varies by architecture).
func unameRelease(u *syscall.Utsname) string {
	b := make([]byte, 0, len(u.Release))
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// kernelAtLeast parses a "MAJOR.MINOR…" release string (e.g. "5.15.0-91-generic")
// and reports whether it is at least major.minor.
func kernelAtLeast(release string, major, minor int) bool {
	var maj, min int
	if n, _ := fmt.Sscanf(release, "%d.%d", &maj, &min); n < 2 {
		return false
	}
	return maj > major || (maj == major && min >= minor)
}

func createCgroup(limits Limits) (*cgroupJob, error) {
	const root = "/sys/fs/cgroup"
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return nil, errors.New("sys: cgroup v2 not mounted")
	}
	// Atomic placement needs clone3 CLONE_INTO_CGROUP (kernel ≥ 5.7). Without it a
	// child could fork a grandchild before joining the cgroup, so rather than ship a
	// weaker containment than the process group, fall back to it on older kernels —
	// some distros run cgroup v2 on 5.3–5.6 (e.g. Fedora 31/32).
	if !cgroupFDSupported() {
		return nil, errors.New("sys: cgroup placement needs clone3 CLONE_INTO_CGROUP (kernel 5.7+)")
	}
	rel, err := selfCgroupRel()
	if err != nil {
		return nil, err
	}
	parent := filepath.Join(root, strings.TrimPrefix(rel, "/"))
	path, err := mkdirUniqueCgroup(parent)
	if err != nil {
		return nil, err
	}
	cg := &cgroupJob{path: path}
	if limits.Any() {
		if err := cg.applyLimits(parent, limits); err != nil {
			_ = os.Remove(path)
			return nil, err
		}
	}
	// Hold an open handle to the cgroup directory for UseCgroupFD atomic placement,
	// capturing the fd once (so concurrent Configure calls don't race on Fd()).
	dir, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	cg.dir = dir
	cg.dirFD = int(dir.Fd())
	return cg, nil
}

// selfCgroupRel reads this process's own cgroup-v2 path from /proc/self/cgroup,
// whose unified-hierarchy line is "0::<path>".
func selfCgroupRel() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rel), nil
		}
	}
	return "/", nil
}

// mkdirUniqueCgroup creates a uniquely-named leaf cgroup under parent. EEXIST (a
// stale dir, or two instances racing) retries with a fresh counter; a real
// permission error (EACCES/EPERM/EROFS — no delegation) is propagated so newJob can
// fall back to a process group.
func mkdirUniqueCgroup(parent string) (string, error) {
	for i := 0; i < 32; i++ {
		name := fmt.Sprintf("processkit-%d-%s-%d", os.Getpid(), cgroupNameSalt(), cgroupCounter.Add(1))
		path := filepath.Join(parent, name)
		switch err := os.Mkdir(path, 0o755); {
		case err == nil:
			return path, nil
		case errors.Is(err, os.ErrExist):
			continue
		default:
			return "", err
		}
	}
	return "", errors.New("sys: could not create a unique cgroup directory after retries")
}

// applyLimits enables the controllers each requested cap needs in the parent's
// cgroup.subtree_control (only the ones not already enabled), then writes the caps.
// Enabling controllers in a cgroup that holds member processes is forbidden by
// cgroup v2's "no internal processes" rule for any non-root cgroup — so this
// succeeds only when the parent is the real cgroup-v2 hierarchy root, and otherwise
// fails loud (the caller maps it to ErrResourceLimit). A cgroup namespace root does
// NOT count, so a container or systemd scope fails here.
func (j *cgroupJob) applyLimits(parent string, limits Limits) error {
	var needed []string
	if limits.HasMemoryMax {
		needed = append(needed, "memory")
	}
	if limits.HasMaxProcesses {
		needed = append(needed, "pids")
	}
	if limits.HasCPUQuota {
		needed = append(needed, "cpu")
	}

	enabled, _ := os.ReadFile(filepath.Join(parent, "cgroup.subtree_control"))
	toEnable := controllersToEnable(needed, string(enabled))
	if len(toEnable) > 0 {
		spec := "+" + strings.Join(toEnable, " +")
		if err := writeCgroup(filepath.Join(parent, "cgroup.subtree_control"), spec); err != nil {
			return fmt.Errorf("sys: enabling cgroup controllers (%s) failed: %w — limits need this process at the real cgroup-v2 root (not under systemd or in a container)", spec, err)
		}
	}

	if limits.HasMemoryMax {
		if err := writeCgroup(filepath.Join(j.path, "memory.max"), strconv.FormatUint(limits.MemoryMax, 10)); err != nil {
			return err
		}
	}
	if limits.HasMaxProcesses {
		if err := writeCgroup(filepath.Join(j.path, "pids.max"), strconv.FormatUint(uint64(limits.MaxProcesses), 10)); err != nil {
			return err
		}
	}
	if limits.HasCPUQuota {
		if err := writeCgroup(filepath.Join(j.path, "cpu.max"), cpuMaxValue(limits.CPUQuota)); err != nil {
			return err
		}
	}
	return nil
}

// controllersToEnable returns the needed controllers not already in subtreeControl,
// in order — so the redundant subtree_control write is skipped where they are
// already delegated (the one way limits work without being the root).
func controllersToEnable(needed []string, subtreeControl string) []string {
	already := make(map[string]bool)
	for _, c := range strings.Fields(subtreeControl) {
		already[c] = true
	}
	var out []string
	for _, c := range needed {
		if !already[c] {
			out = append(out, c)
		}
	}
	return out
}

// cpuMaxValue formats a per-core CPU quota as a cgroup v2 cpu.max value
// ("quota period" microseconds): 0.5 → "50000 100000", 2.0 → "200000 100000". The
// quota floors at 1 (a zero quota is invalid).
func cpuMaxValue(cores float64) string {
	const period = 100000
	// Clamp an absurd-but-finite quota to a bound exactly representable as both a
	// float64 and an int64 (2^62 ≈ 4.6e18 µs — effectively unlimited), so the cast
	// can't overflow. (float64(math.MaxInt64) rounds UP to MaxInt64+1, which would.)
	const maxQuota = float64(1 << 62)
	q := math.Round(cores * period)
	if q < 1 {
		q = 1
	}
	if q > maxQuota {
		q = maxQuota
	}
	return fmt.Sprintf("%d %d", int64(q), period)
}

// writeCgroup writes content to a cgroup control file, opening it write-only (these
// are kernel virtual files, so no create/truncate).
func writeCgroup(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, werr := f.Write([]byte(content))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func addDurationSat(a, b time.Duration) time.Duration {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func addU64Sat(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

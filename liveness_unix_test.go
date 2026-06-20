//go:build unix

package processkit

import (
	"bytes"
	"errors"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
)

// processAlive reports whether a process with the given pid is running. Signal 0
// probes without delivering: ESRCH means gone, EPERM means it exists but isn't
// ours. A killed-but-unreaped child is a zombie — effectively dead — so on Linux
// we additionally treat the zombie state (from /proc) as not alive, which keeps
// the kill-on-drop tests reliable even where no init reaps orphans promptly.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			// Format: "pid (comm) state …"; comm may contain ')' so scan to the
			// last one, then the state is two bytes later. 'Z' is a zombie.
			if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'Z' {
				return false
			}
		}
	}
	return true
}

// selfSig (helper mode "selfsig") kills the current process with SIGKILL, so the
// run captures a Signalled outcome.
func selfSig() { _ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL) }

// termExit (helper mode "termexit") exits cleanly on SIGTERM — so a graceful
// Group.Shutdown leaves it Exited(0), not SIGKILLed.
func termExit() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	<-ch
	os.Exit(0)
}

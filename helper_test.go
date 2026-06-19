package processkit

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain doubles the test binary as a subprocess helper: when PK_HELPER is set
// it acts as the requested helper and exits, otherwise it runs the test suite.
// This gives cross-platform real-subprocess fixtures without depending on any
// external program being on PATH.
func TestMain(m *testing.M) {
	switch os.Getenv("PK_HELPER") {
	case "":
		os.Exit(m.Run())
	case "exit":
		if s, ok := os.LookupEnv("PK_STDOUT"); ok {
			fmt.Fprint(os.Stdout, s)
		}
		if s, ok := os.LookupEnv("PK_STDERR"); ok {
			fmt.Fprint(os.Stderr, s)
		}
		os.Exit(envInt("PK_CODE", 0))
	case "tree":
		// Spawn a grandchild that lingers, report its pid, then exit cleanly. The
		// grandchild outlives us, so the run's job teardown is what must reap it.
		exe, err := os.Executable()
		if err != nil {
			os.Exit(97)
		}
		g := exec.Command(exe)
		g.Env = append(cleanEnv(), "PK_HELPER=sleep")
		if err := g.Start(); err != nil {
			os.Exit(98)
		}
		fmt.Printf("grandchild=%d\n", g.Process.Pid)
		os.Exit(0)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "linethensleep":
		fmt.Println("before-timeout") // written before the deadline kills us
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "selfsig":
		selfSig() // Unix: SIGKILL self → Signalled outcome. Windows: os.Exit(42).
		os.Exit(43)
	default:
		os.Exit(99)
	}
}

func envInt(key string, def int) int {
	if s, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

// cleanEnv returns the parent environment with PK_HELPER removed, so a helper we
// spawn doesn't inherit (and re-trigger) the parent's mode.
func cleanEnv() []string {
	var out []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PK_HELPER=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// helperEnv builds a full environment that runs the test binary as the given
// helper mode, plus any extra "KEY=VALUE" entries.
func helperEnv(mode string, extra ...string) []string {
	env := append(cleanEnv(), "PK_HELPER="+mode)
	return append(env, extra...)
}

// selfExe is the path to the test binary, used to re-exec helpers.
func selfExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

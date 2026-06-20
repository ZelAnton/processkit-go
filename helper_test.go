package processkit

import (
	"bufio"
	"fmt"
	"io"
	"net"
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
	case "groupchild":
		// Spawn a lingering grandchild, record its pid to PK_PIDFILE, then linger —
		// so a Group.Close must reap the grandchild via the shared container.
		exe, err := os.Executable()
		if err != nil {
			os.Exit(97)
		}
		gc := exec.Command(exe)
		gc.Env = append(cleanEnv(), "PK_HELPER=sleep")
		if err := gc.Start(); err != nil {
			os.Exit(98)
		}
		if pf := os.Getenv("PK_PIDFILE"); pf != "" {
			_ = os.WriteFile(pf, []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "emitlines":
		// Emit PK_LINES lines to stdout ("out N"), and to stderr ("err N") every
		// PK_STDERR_EVERY-th line, sleeping PK_DELAY_MS between lines. Used to drive
		// the streaming tests with deterministic, ordered output.
		n := envInt("PK_LINES", 3)
		stderrEvery := envInt("PK_STDERR_EVERY", 0)
		delay := time.Duration(envInt("PK_DELAY_MS", 0)) * time.Millisecond
		for i := 1; i <= n; i++ {
			fmt.Fprintf(os.Stdout, "out %d\n", i)
			if stderrEvery > 0 && i%stderrEvery == 0 {
				fmt.Fprintf(os.Stderr, "err %d\n", i)
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
		os.Exit(0)
	case "catlines":
		// Echo each line of stdin to stdout as "echo: <line>" until EOF, then exit.
		// Drives the interactive-stdin test.
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			fmt.Fprintf(os.Stdout, "echo: %s\n", sc.Text())
		}
		os.Exit(0)
	case "upper":
		// Read all of stdin, upper-case ASCII, write to stdout. A pipeline transform.
		b, _ := io.ReadAll(os.Stdin)
		for i := range b {
			if c := b[i]; c >= 'a' && c <= 'z' {
				b[i] = c - 32
			}
		}
		_, _ = os.Stdout.Write(b)
		os.Exit(0)
	case "drainexit":
		// Drain stdin, emit fixed stdout/stderr, exit with PK_CODE. A pipeline stage
		// whose exit code drives pipefail attribution.
		_, _ = io.Copy(io.Discard, os.Stdin)
		if s, ok := os.LookupEnv("PK_STDOUT"); ok {
			fmt.Fprint(os.Stdout, s)
		}
		if s, ok := os.LookupEnv("PK_STDERR"); ok {
			fmt.Fprint(os.Stderr, s)
		}
		os.Exit(envInt("PK_CODE", 0))
	case "headone":
		// Print the first line of stdin, then exit — leaving the rest unread, so an
		// upstream producer is killed by SIGPIPE (Unix) or a broken pipe (Windows).
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() {
			fmt.Println(sc.Text())
		}
		os.Exit(0)
	case "readyline":
		// Announce readiness on stdout (PK_READY, default "server ready"), then
		// linger. Drives WaitForLine.
		msg := os.Getenv("PK_READY")
		if msg == "" {
			msg = "server ready"
		}
		fmt.Println(msg)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "listen":
		// Bind an ephemeral TCP port, announce it as "PORT=host:port", accept
		// connections, then linger. Drives WaitForPort.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(91)
		}
		fmt.Printf("PORT=%s\n", ln.Addr().String())
		go func() {
			for {
				c, e := ln.Accept()
				if e != nil {
					return
				}
				_ = c.Close()
			}
		}()
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "flakyfile":
		// Increment a counter in the PK_COUNTER file; exit 1 until the count reaches
		// PK_SUCCEED_AT, then print "ok" and exit 0. Drives a real retry-to-success
		// test (persistent state across re-execs via the file).
		path := os.Getenv("PK_COUNTER")
		n := 0
		if b, err := os.ReadFile(path); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		n++
		_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600)
		if n < envInt("PK_SUCCEED_AT", 3) {
			os.Exit(1)
		}
		fmt.Println("ok")
		os.Exit(0)
	case "termexit":
		termExit() // Unix: exit 0 on SIGTERM (graceful). Windows: exit 0.
		os.Exit(44)
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

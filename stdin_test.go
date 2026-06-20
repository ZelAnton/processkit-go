package processkit

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestCmd_WithStdin(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	// The "upper" helper reads all of stdin, upper-cases ASCII, and writes it out.
	res, err := Command(selfExe(t)).WithEnv(helperEnv("upper")...).
		WithStdin(strings.NewReader("hello, world")).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if res.Stdout() != "HELLO, WORLD" {
		t.Errorf("Stdout = %q, want %q", res.Stdout(), "HELLO, WORLD")
	}
}

func TestCmd_WithStdin_Run(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	out, err := Command(selfExe(t)).WithEnv(helperEnv("upper")...).
		WithStdin(strings.NewReader("abc\n")).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "ABC" { // Run trims trailing whitespace
		t.Errorf("Run = %q, want %q", out, "ABC")
	}
}

func TestCmd_WithStdin_NoMutate(t *testing.T) {
	base := Command("tool")
	_ = base.WithStdin(strings.NewReader("x"))
	if base.invocation().Stdin != nil {
		t.Error("WithStdin must not mutate the receiver (copy-on-write)")
	}
}

func TestCmd_NoStdinIsNil(t *testing.T) {
	if Command("tool").invocation().Stdin != nil {
		t.Error("a command without WithStdin should have a nil Invocation.Stdin")
	}
}

func TestCmd_WithStdinString(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess test")
	}
	res, err := Command(selfExe(t)).WithEnv(helperEnv("upper")...).
		WithStdinString("hi there").Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if res.Stdout() != "HI THERE" {
		t.Errorf("Stdout = %q, want %q", res.Stdout(), "HI THERE")
	}
}

// WithStdinBytes/String are re-readable: invocation() yields a fresh, fully-readable
// reader each call, so a retry or supervisor restart feeds the full input again.
func TestCmd_WithStdinBytes_ReReadable(t *testing.T) {
	c := Command("tool").WithStdinBytes([]byte("data"))
	readAll := func() string {
		b, _ := io.ReadAll(c.invocation().Stdin)
		return string(b)
	}
	if first, second := readAll(), readAll(); first != "data" || second != "data" {
		t.Errorf("WithStdinBytes should re-read fully each time: first=%q second=%q", first, second)
	}
}

// WithStdin is one-shot: the same reader is handed out, so a second run sees EOF.
func TestCmd_WithStdin_OneShot(t *testing.T) {
	c := Command("tool").WithStdin(strings.NewReader("data"))
	first, _ := io.ReadAll(c.invocation().Stdin)
	second, _ := io.ReadAll(c.invocation().Stdin)
	if string(first) != "data" || len(second) != 0 {
		t.Errorf("WithStdin should be one-shot: first=%q second=%q", first, second)
	}
}

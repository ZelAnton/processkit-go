package processkit

import (
	"context"
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

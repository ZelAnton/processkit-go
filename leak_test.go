package processkit

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestJobRunner_ReapsGrandchild is the kill-on-drop contract: a grandchild
// spawned by the direct child must be reaped when the run's job is torn down. The
// "tree" helper forks a lingering grandchild then exits cleanly; on completion the
// run's job teardown must still reap the grandchild.
func TestJobRunner_ReapsGrandchild(t *testing.T) {
	if testing.Short() {
		t.Skip("real-subprocess / kill-on-drop test")
	}
	res, err := Command(selfExe(t)).WithEnv(helperEnv("tree")...).Output(context.Background())
	if err != nil {
		t.Fatalf("Output: %v", err)
	}

	pid := grandchildPid(t, res.Stdout())
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive after teardown — orphan leak (mechanism=%v)", pid, res.Mechanism())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func grandchildPid(t *testing.T, stdout string) int {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "grandchild="); ok {
			pid, err := strconv.Atoi(rest)
			if err != nil {
				t.Fatalf("bad grandchild pid %q: %v", rest, err)
			}
			return pid
		}
	}
	t.Fatalf("no grandchild pid in stdout: %q", stdout)
	return 0
}

//go:build windows

package sys

import "testing"

func TestCPUHardCapRate(t *testing.T) {
	cases := []struct {
		cores, cpus float64
		want        uint32
	}{
		{0.5, 4, 1250},  // half a core of four → 12.5% of total → 1250
		{2, 4, 5000},    // two cores of four → 50% → 5000
		{8, 4, 10000},   // more cores than exist → saturates at 100%
		{1, 1, 10000},   // a whole single-core host → 100%
		{0.0001, 64, 1}, // rounds to ~0 but floors at 1 (a zero rate is rejected)
		{1, 0, 10000},   // a nonsense zero cpu count is treated as 1 → 100%
	}
	for _, c := range cases {
		if got := cpuHardCapRate(c.cores, c.cpus); got != c.want {
			t.Errorf("cpuHardCapRate(%v, %v) = %d, want %d", c.cores, c.cpus, got, c.want)
		}
	}
}

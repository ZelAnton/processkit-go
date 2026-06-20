package processkit

import "testing"

// The Mechanism string values are part of the observable surface — lock them, the
// reserved CgroupV2 case included.
func TestMechanism_String(t *testing.T) {
	cases := map[Mechanism]string{
		MechanismUnknown:      "Unknown",
		MechanismJobObject:    "JobObject",
		MechanismProcessGroup: "ProcessGroup",
		MechanismCgroupV2:     "CgroupV2",
		Mechanism(99):         "Unknown", // any unmapped value
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("Mechanism(%d).String() = %q, want %q", int(m), got, want)
		}
	}
}

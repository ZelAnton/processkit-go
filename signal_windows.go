//go:build windows

package processkit

// number is unused on Windows: only [SignalKill] is deliverable there (routed to
// the atomic job kill, not through this number), and every other signal is
// [ErrUnsupported]. It exists so the cross-platform [Group.Signal] compiles.
func (s Signal) number() int { return s.raw }

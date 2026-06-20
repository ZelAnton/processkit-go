package processkit

import "fmt"

// Signal is a portable signal to deliver to a [Group] with [Group.Signal]. Use a
// curated value ([SignalTerm], [SignalKill], …) for portability, or [RawSignal] to
// pass a raw Unix signal number. Only [SignalKill] is honoured on every platform;
// the rest are Unix-only (Windows returns [ErrUnsupported]).
type Signal struct {
	name string
	kind sigKind
	raw  int // the signal number for a RawSignal
}

type sigKind uint8

const (
	sigTerm sigKind = iota
	sigKill
	sigInt
	sigHup
	sigQuit
	sigUsr1
	sigUsr2
	sigRaw
)

// The curated signals. They map to the platform's matching syscall signal on Unix;
// on Windows only [SignalKill] is deliverable (it routes to the atomic job kill).
var (
	SignalTerm = Signal{name: "TERM", kind: sigTerm} // graceful stop (SIGTERM)
	SignalKill = Signal{name: "KILL", kind: sigKill} // hard kill (SIGKILL; the only portable signal)
	SignalInt  = Signal{name: "INT", kind: sigInt}   // interrupt (SIGINT)
	SignalHup  = Signal{name: "HUP", kind: sigHup}   // hang up / reload (SIGHUP)
	SignalQuit = Signal{name: "QUIT", kind: sigQuit} // quit with core (SIGQUIT)
	SignalUsr1 = Signal{name: "USR1", kind: sigUsr1} // user-defined 1 (SIGUSR1)
	SignalUsr2 = Signal{name: "USR2", kind: sigUsr2} // user-defined 2 (SIGUSR2)
)

// RawSignal is the escape hatch for a raw Unix signal number not in the curated
// set (e.g. SIGWINCH). It is always [ErrUnsupported] on Windows.
func RawSignal(n int) Signal { return Signal{kind: sigRaw, raw: n} }

// String renders the signal, e.g. "SIGTERM" or "signal 28".
func (s Signal) String() string {
	if s.kind == sigRaw {
		return fmt.Sprintf("signal %d", s.raw)
	}
	return "SIG" + s.name
}

// isKill reports whether this is the universally-deliverable kill signal.
func (s Signal) isKill() bool { return s.kind == sigKill }

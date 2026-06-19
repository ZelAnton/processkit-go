// Package processkit will hold the public API of processkit-go — kernel-backed,
// no-orphan child-process trees (a native Go reimplementation of ProcessKit-rs).
//
// The Greet stub below is a temporary placeholder from the project scaffold; it
// is replaced by the real Command / Group surface as Phase 1 lands (see AGENTS.md
// and the HQ ROADMAP).
package processkit

import "fmt"

// Greet returns a friendly greeting for name.
func Greet(name string) string {
	return fmt.Sprintf("Hello, %s!", name)
}

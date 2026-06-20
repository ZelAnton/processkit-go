//go:build unix

package processkittest

import (
	"os"
	"syscall"
)

// writeFileSecure writes data to path owner-only (0600) and refuses to follow a
// symlink at path (O_NOFOLLOW), so a planted symlink can't redirect the
// secret-bearing write (and its 0600) onto the link's target — it fails loud
// instead. A truncating open of a pre-existing, world-readable cassette keeps the
// old permissions, so we chmod to 0600 AFTER the (emptying) open and BEFORE the
// secret bytes are written.
func writeFileSecure(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // tighten even a pre-existing file, while it is empty
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

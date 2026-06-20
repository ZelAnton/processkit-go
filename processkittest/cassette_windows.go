//go:build windows

package processkittest

import "os"

// writeFileSecure writes data to path. On Windows the Unix mode bits are advisory
// and there is no O_NOFOLLOW equivalent, so the cassette inherits the containing
// directory's ACL (the unit of access control there). Keep the fixture directory
// restricted — or use a per-user temp dir, not a world-writable shared one — if a
// cassette can carry secrets in its verbatim argv / cwd / stdout / stderr.
func writeFileSecure(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

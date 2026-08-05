//go:build !windows

package service

import (
	"os"
	"syscall"
)

// openKeyFile opens a key file, refusing to follow a symlink.
//
// O_NOFOLLOW makes the refusal atomic: the kernel fails the open rather than
// HarborMaster checking the path and then opening it, which would leave a
// window in which the two refer to different files.
//
// A symlinked key file is a redirection primitive, not a configuration style.
// Following one would let anything able to write in the key's directory point
// HarborMaster at a key of its choosing, and digests produced under an
// attacker-known key can be compared against a wordlist offline.
//
// gosec flags the variable path (G304). It is a false positive here, and the
// reason is worth stating rather than suppressing silently: `path` is the
// operator's own HARBORMASTER_SNAPSHOT_HMAC_KEY_FILE, read from the process
// environment at startup. It never comes from a request, a Docker payload, or
// the database. There is no untrusted input on this path to traverse with.
//
// The risk G304 exists to catch -- being redirected to a file you did not mean
// to open -- is the one O_NOFOLLOW closes, atomically, one line below. This is
// the pattern secure-coding.md 9.1 mandates, so the finding fires on the
// mitigation itself.
//
//nolint:gosec // G304: path is operator configuration, and O_NOFOLLOW is the control.
func openKeyFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

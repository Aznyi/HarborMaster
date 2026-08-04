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
func openKeyFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

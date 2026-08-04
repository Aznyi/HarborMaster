//go:build windows

package service

import (
	"fmt"
	"os"
)

// openKeyFile opens a key file, refusing to follow a symlink or reparse point.
//
// Windows has no O_NOFOLLOW, so the refusal cannot be atomic the way it is on
// Unix: this checks the link status and then opens the path. The window between
// the two is real, and an attacker who can replace the path with a symlink
// inside it would still be followed.
//
// The narrower guarantee is acceptable because the threat it defends against --
// an attacker with write access to the key's own directory -- already implies
// substantial local access on a platform where HarborMaster is a development
// target rather than the deployment form. The Unix build, which is what ships in
// the container image, gets the atomic check.
func openKeyFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink", path)
	}
	if info.Mode()&os.ModeIrregular != 0 {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.Open(path)
}

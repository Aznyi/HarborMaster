//go:build !windows

package store

// posixPermissionsEnforced reports whether os.FileMode's permission bits are
// the real access control on this platform.
//
// True everywhere HarborMaster is deployed for real: the release image is
// Linux, and the mode bits are what the kernel checks on every open.
func posixPermissionsEnforced() bool { return true }

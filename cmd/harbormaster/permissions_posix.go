//go:build !windows

package main

// posixPermissionsMeaningful reports whether os.FileMode's permission bits
// describe the real access control on this platform.
//
// True everywhere HarborMaster is deployed for real: the release image is
// Linux, and the mode bits are what the kernel enforces.
func posixPermissionsMeaningful() bool { return true }

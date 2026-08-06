//go:build windows

package main

// posixPermissionsMeaningful reports whether os.FileMode's permission bits
// describe the real access control on this platform.
//
// False on Windows. Go synthesises a mode from the read-only attribute alone,
// so a file's bits there say nothing about who can read it -- the answer lives
// in an ACL this program does not read. Reporting "0600, looks fine" from a
// synthesised value would be a security check that lies, which is worse than
// none: the console commands say the check was skipped instead.
func posixPermissionsMeaningful() bool { return false }

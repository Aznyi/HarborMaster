//go:build windows

package store

// posixPermissionsEnforced reports whether os.FileMode's permission bits are
// the real access control on this platform.
//
// False on Windows. Go synthesises a mode from the read-only attribute alone,
// so chmod'ing to 0600 there would change nothing an attacker has to get past:
// the answer lives in an ACL this package does not read or write.
//
// Reporting "restricted to the owner" from a value the operating system does
// not enforce would be a security control that lies, which is worse than an
// absent one — an operator would stop looking. So the pass is skipped, and
// PermissionReport.Enforced says it was skipped rather than claiming success.
//
// This mirrors posixPermissionsMeaningful in cmd/harbormaster, which makes the
// same distinction for the console commands' warnings.
func posixPermissionsEnforced() bool { return false }

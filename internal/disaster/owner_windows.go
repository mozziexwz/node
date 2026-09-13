//go:build windows

package disaster

import "os"

// Production entrypoints require Debian 12 root. Windows only runs fixtures;
// Unix mode/ownership assertions run separately in Linux CI.
func ownedByCurrentUser(info os.FileInfo) bool { return true }
func safeAncestor(info os.FileInfo) bool       { return true }

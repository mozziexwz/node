//go:build !windows

package disaster

import (
	"os"
	"syscall"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func safeAncestor(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() && stat.Uid != 0 {
		return false
	}
	return info.Mode().Perm()&0022 == 0 || stat.Uid == 0 && info.Mode()&os.ModeSticky != 0
}

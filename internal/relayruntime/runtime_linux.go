//go:build linux

package relayruntime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Offline retention does not detach process ownership: a crashed/killed Agent
// must not leave an unmanaged GOST child. Agent restart is not TCP continuity.
func protectChild(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }

// One trusted state directory belongs to exactly one live Agent. Never unlink
// this file: removing a locked inode would let another Agent lock its replacement.
func lockV2StateDir(path string) (func(), error) {
	lockPath := filepath.Join(path, "relay-v2.lock")
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errors.New("cannot open private v2 Agent lock")
	}
	file := os.NewFile(uintptr(fd), lockPath)
	reject := func(message string) (func(), error) {
		_ = file.Close()
		return nil, errors.New(message)
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&07777 != 0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return reject("v2 Agent lock must be an owned private regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return reject("v2 Agent state directory is already locked or unavailable")
	}
	opened, err := file.Stat()
	current, currentErr := os.Lstat(lockPath)
	if err != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return reject("v2 Agent lock path changed while opening")
	}
	return func() { _ = file.Close() }, nil
}

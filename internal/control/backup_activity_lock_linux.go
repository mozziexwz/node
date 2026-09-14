//go:build linux

package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type backupDirectoryIdentity struct {
	device uint64
	inode  uint64
}

// Open every component relative to its already-open parent. A string-based
// Lstat/Open sequence alone could follow a swapped parent symlink. Keep the
// final directory descriptor until the file and complete path are rechecked.
func openBackupActivityDirectory(path string) (int, []backupDirectoryIdentity, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return -1, nil, err
	}
	var identities []backupDirectoryIdentity
	components := append([]string{""}, strings.Split(strings.TrimPrefix(path, "/"), "/")...)
	for index, component := range components {
		if index != 0 {
			next, openErr := unix.Openat(fd, component, flags, 0)
			_ = unix.Close(fd)
			if openErr != nil {
				return -1, nil, fmt.Errorf("backup data directory cannot contain missing or symlinked components: %w", openErr)
			}
			fd = next
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return -1, nil, err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(fd)
			return -1, nil, errors.New("backup data path component is not a directory")
		}
		identities = append(identities, backupDirectoryIdentity{uint64(stat.Dev), stat.Ino})
	}
	return fd, identities, nil
}

func validateBackupActivityFile(stat *unix.Stat_t, owner uint32) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&07777 != 0600 || stat.Uid != owner || stat.Nlink != 1 {
		return errors.New("backup activity lock must be a single-link regular file, mode 0600, owned by the data directory owner")
	}
	return nil
}

func validateHeldBackupActivityLock(path string, originalPath []backupDirectoryIdentity, owner uint32, file *os.File, identity string) error {
	checkFD, currentPath, err := openBackupActivityDirectory(path)
	if err != nil {
		return err
	}
	defer unix.Close(checkFD)
	if len(currentPath) != len(originalPath) {
		return errors.New("backup data directory path changed while holding its lock")
	}
	for index := range originalPath {
		if originalPath[index] != currentPath[index] {
			return errors.New("backup data directory path changed while holding its lock")
		}
	}
	var directory, opened, current unix.Stat_t
	if err := unix.Fstat(checkFD, &directory); err != nil {
		return err
	}
	if directory.Uid != owner || directory.Mode&0022 != 0 {
		return errors.New("backup data directory ownership or permissions changed while holding its lock")
	}
	if err := unix.Fstatat(checkFD, backupActivityLockName, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return err
	}
	if err := validateBackupActivityFile(&opened, owner); err != nil {
		return err
	}
	if err := validateBackupActivityFile(&current, owner); err != nil {
		return err
	}
	if current.Dev != opened.Dev || current.Ino != opened.Ino || current.Size != 65 || opened.Size != 65 {
		return errors.New("backup activity lock inode changed while holding it")
	}
	currentIdentity, err := readBackupActivityIdentity(file)
	if err != nil {
		return err
	}
	if currentIdentity != identity {
		return errors.New("backup activity lock nonce changed while holding it")
	}
	return nil
}

// create only permits initial creation of the lock file; the data directory
// must already exist. A read-only recovery helper uses create=false and may run
// as root with CAP_DAC_READ_SEARCH against the original read-only app_data mount.
// It never chmods, chowns, writes, recreates, or unlinks the existing lock file.
func acquireBackupActivityLock(dataDir string, create bool) (*backupActivityLock, error) {
	path, err := filepath.Abs(dataDir)
	if err != nil || dataDir == "" || path == "/" {
		return nil, errors.New("backup activity lock requires an explicit existing data directory")
	}
	fd, originalPath, err := openBackupActivityDirectory(path)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var directory unix.Stat_t
	if err := unix.Fstat(fd, &directory); err != nil {
		return nil, err
	}
	uid := uint32(os.Geteuid())
	if directory.Mode&0022 != 0 || (directory.Uid != uid && !(uid == 0 && directory.Uid == 10001)) || (create && directory.Uid != uid) {
		return nil, errors.New("backup data directory must be owned by the application UID and not group/world writable; root may only inspect existing official UID 10001 locks")
	}
	flags := unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	created := false
	var lockFD int
	if create {
		lockFD, err = unix.Openat(fd, backupActivityLockName, flags|unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
		if errors.Is(err, unix.EEXIST) {
			lockFD, err = unix.Openat(fd, backupActivityLockName, flags|unix.O_RDWR, 0)
		} else if err == nil {
			created = true
		}
	} else {
		lockFD, err = unix.Openat(fd, backupActivityLockName, flags|unix.O_RDONLY, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot open original backup activity lock: %w", err)
	}
	file := os.NewFile(uintptr(lockFD), filepath.Join(path, backupActivityLockName))
	success := false
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	if created {
		// O_EXCL proves this is our newly created inode. Do not adjust existing
		// files; a restrictive umask must not leave our own new file unreadable.
		if err := file.Chmod(0600); err != nil {
			return nil, err
		}
	}
	var opened unix.Stat_t
	if err := unix.Fstat(lockFD, &opened); err != nil {
		return nil, err
	}
	if err := validateBackupActivityFile(&opened, directory.Uid); err != nil {
		return nil, err
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errBackupActivityLocked
		}
		return nil, fmt.Errorf("cannot acquire exclusive backup activity lock: %w", err)
	}
	if created {
		if err := writeBackupActivityIdentity(file); err != nil {
			return nil, err
		}
		if err := unix.Fsync(fd); err != nil {
			return nil, fmt.Errorf("cannot persist backup activity lock directory: %w", err)
		}
	}
	identity, err := readBackupActivityIdentity(file)
	if err != nil {
		return nil, err
	}
	validate := func() error { return validateHeldBackupActivityLock(path, originalPath, directory.Uid, file, identity) }
	if err := validate(); err != nil {
		return nil, err
	}
	success = true
	// Closing the only descriptor releases flock, including on process death.
	// Never unlink: another process must contend on this same persistent inode.
	return &backupActivityLock{Identity: identity, Device: strconv.FormatUint(uint64(opened.Dev), 10), Inode: strconv.FormatUint(opened.Ino, 10), close: file.Close, validate: validate}, nil
}

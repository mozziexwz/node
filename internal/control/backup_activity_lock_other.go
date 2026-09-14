//go:build !linux

package control

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Non-Linux support is an in-process local-test simulation only. Production
// recovery is Linux-only and must use flock. An O_EXCL sentinel is not a lock:
// after a crash it would remain forever and cannot prove whether work is alive.
var simulatedBackupActivityLocks = struct {
	sync.Mutex
	held map[string]bool
}{held: make(map[string]bool)}

type simulatedBackupActivityDirectory struct {
	path            string
	info            os.FileInfo
	compareIdentity bool
}

func validateSimulatedBackupActivityLock(path string, parents []simulatedBackupActivityDirectory, file *os.File, identity string) error {
	for _, parent := range parents {
		current, err := os.Lstat(parent.path)
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || (parent.compareIdentity && !os.SameFile(parent.info, current)) {
			return fmt.Errorf("backup data directory path changed while holding its simulated lock: %s", parent.path)
		}
	}
	opened, err := file.Stat()
	current, currentErr := os.Lstat(path)
	if err != nil || currentErr != nil || !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return errors.New("backup activity lock path changed while holding it")
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

func acquireBackupActivityLock(dataDir string, create bool) (*backupActivityLock, error) {
	path, err := filepath.Abs(dataDir)
	if err != nil || dataDir == "" || filepath.Dir(path) == path {
		return nil, errors.New("backup activity lock requires an explicit existing data directory")
	}
	var parents []simulatedBackupActivityDirectory
	for component := path; ; component = filepath.Dir(component) {
		info, err := os.Lstat(component)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("backup data directory must exist without symlinked components")
		}
		// Windows desktop sandboxes can allow Lstat but deny opening an ancestor
		// (such as the home directory) to retrieve its file ID. Only the local
		// simulation tolerates that ancestor-ID limitation; the data directory
		// itself and lock file must remain comparable. Linux never relaxes it.
		compareIdentity := os.SameFile(info, info)
		if component == path && !compareIdentity {
			return nil, errors.New("cannot inspect the local simulated backup data directory identity")
		}
		parents = append(parents, simulatedBackupActivityDirectory{path: component, info: info, compareIdentity: compareIdentity})
		if filepath.Dir(component) == component {
			break
		}
	}
	key := path
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	simulatedBackupActivityLocks.Lock()
	if simulatedBackupActivityLocks.held[key] {
		simulatedBackupActivityLocks.Unlock()
		return nil, errBackupActivityLocked
	}
	simulatedBackupActivityLocks.held[key] = true
	simulatedBackupActivityLocks.Unlock()
	release := func() {
		simulatedBackupActivityLocks.Lock()
		delete(simulatedBackupActivityLocks.held, key)
		simulatedBackupActivityLocks.Unlock()
	}
	success := false
	defer func() {
		if !success {
			release()
		}
	}()
	lockPath := filepath.Join(path, backupActivityLockName)
	if info, err := os.Lstat(lockPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("backup activity lock must be a regular file without symlinks")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var file *os.File
	created := false
	if create {
		file, err = os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			file, err = os.OpenFile(lockPath, os.O_RDWR, 0)
		} else if err == nil {
			created = true
		}
	} else {
		file, err = os.Open(lockPath)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()
	if created {
		if err := writeBackupActivityIdentity(file); err != nil {
			return nil, err
		}
	}
	identity, err := readBackupActivityIdentity(file)
	if err != nil {
		return nil, err
	}
	validate := func() error { return validateSimulatedBackupActivityLock(lockPath, parents, file, identity) }
	if err := validate(); err != nil {
		return nil, err
	}
	success = true
	// A stable path identity is sufficient for local in-process tests, not for
	// proving that an inode was not replaced. Never use this simulation to
	// authorize a production recovery operation.
	pathHash := sha256.Sum256([]byte(key))
	return &backupActivityLock{Identity: identity, Device: "simulated-" + runtime.GOOS, Inode: hex.EncodeToString(pathHash[:]), validate: validate, close: func() error {
		err := file.Close()
		release()
		return err
	}}, nil
}

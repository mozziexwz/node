package control

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
)

const backupActivityLockName = "backup-activity.lock"

var errBackupActivityLocked = errors.New("backup activity is already locked by a live operation")

// The identity belongs to a persistent inode, not to an individual operation.
// Operation records bind this identity to their own random operation ID. The
// lock file must never be unlinked, replaced, or repaired automatically: doing
// so could manufacture an apparently unlocked replacement for a live inode.
type backupActivityLock struct {
	Identity string
	Device   string
	Inode    string
	close    func() error
	validate func() error
	once     sync.Once
	closeErr error
}

// Validate rechecks the original path, inode, and nonce while the lock is held.
// Call it again inside the final database transaction after waiting for its
// row lock; an observation taken before that wait is not sufficient evidence.
func (l *backupActivityLock) Validate() error {
	if l == nil || l.validate == nil {
		return errors.New("backup activity lock has no live validation evidence")
	}
	return l.validate()
}

func (l *backupActivityLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() { l.closeErr = l.close() })
	return l.closeErr
}

func writeBackupActivityIdentity(file *os.File) error {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	contents := hex.EncodeToString(random) + "\n"
	if n, err := file.WriteString(contents); err != nil {
		return err
	} else if n != len(contents) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func readBackupActivityIdentity(file *os.File) (string, error) {
	// Read one byte beyond the canonical representation to reject trailing data
	// without allocating according to an untrusted file's size.
	var contents [66]byte
	n, err := file.ReadAt(contents[:], 0)
	if (err != nil && err != io.EOF) || n != 65 || contents[64] != '\n' {
		return "", errors.New("backup activity lock identity is missing or malformed; automatic replacement is forbidden")
	}
	identity := string(contents[:64])
	for _, c := range identity {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return "", errors.New("backup activity lock identity is not canonical lowercase hexadecimal")
		}
	}
	return identity, nil
}

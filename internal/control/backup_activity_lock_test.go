package control

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupActivityLockPersistentIdentityAndExclusion(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireBackupActivityLock(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if len(first.Identity) != 64 || first.Device == "" || first.Inode == "" {
		t.Fatalf("missing persistent lock identity: %#v", first)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("new live lock does not validate: %v", err)
	}
	if second, err := acquireBackupActivityLock(dir, true); !errors.Is(err, errBackupActivityLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("live lock allowed a second writer: %v", err)
	}
	if second, err := acquireBackupActivityLock(dir, false); !errors.Is(err, errBackupActivityLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("live lock allowed read-only inspection: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close is not idempotent: %v", err)
	}
	if err := first.Validate(); err == nil {
		t.Fatal("closed lock still supplied live validation evidence")
	}
	contentsBefore, err := os.ReadFile(filepath.Join(dir, backupActivityLockName))
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireBackupActivityLock(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Identity != first.Identity || second.Device != first.Device || second.Inode != first.Inode {
		t.Fatal("read-only reacquisition did not preserve original identity and inode")
	}
	contentsAfter, err := os.ReadFile(filepath.Join(dir, backupActivityLockName))
	if err != nil || string(contentsAfter) != string(contentsBefore) {
		t.Fatalf("read-only inspection changed the original file: %v", err)
	}
}

func TestBackupActivityLockValidateDetectsChangedNonce(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireBackupActivityLock(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	replacement := strings.Repeat("a", 64)
	if replacement == lock.Identity {
		replacement = strings.Repeat("b", 64)
	}
	if err := os.WriteFile(filepath.Join(dir, backupActivityLockName), []byte(replacement+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Validate(); err == nil {
		t.Fatal("live validation accepted a changed nonce")
	}
}

func TestBackupActivityLockNeverRepairsMissingOrMalformedState(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		if lock, err := acquireBackupActivityLock(dir, false); err == nil {
			_ = lock.Close()
			t.Fatal("read-only inspection created a missing lock")
		}
		if _, err := os.Lstat(filepath.Join(dir, backupActivityLockName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspection manufactured a replacement file: %v", err)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing")
		for _, create := range []bool{false, true} {
			if lock, err := acquireBackupActivityLock(path, create); err == nil {
				_ = lock.Close()
				t.Fatalf("create=%v manufactured a missing data directory", create)
			}
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspection created a replacement directory: %v", err)
		}
	})
	for _, contents := range []string{"", "broken\n", strings.Repeat("a", 64), strings.Repeat("A", 64) + "\n", strings.Repeat("a", 64) + "\nextra", strings.Repeat("g", 64) + "\n"} {
		t.Run("malformed-"+strings.ReplaceAll(contents, "\n", "-")+"-end", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, backupActivityLockName)
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			for _, create := range []bool{false, true} {
				if lock, err := acquireBackupActivityLock(dir, create); err == nil {
					_ = lock.Close()
					t.Fatalf("create=%v accepted malformed identity", create)
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != contents {
					t.Fatalf("create=%v repaired an existing malformed file: %v", create, err)
				}
			}
		})
	}
}

func TestBackupActivityLockRejectsNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, backupActivityLockName), 0700); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		if lock, err := acquireBackupActivityLock(dir, create); err == nil {
			_ = lock.Close()
			t.Fatalf("create=%v accepted a directory as a lock file", create)
		}
	}
}

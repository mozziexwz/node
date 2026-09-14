//go:build linux

package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupActivityLockProcessHelper(t *testing.T) {
	if os.Getenv("MSBOOST_BACKUP_LOCK_CHILD") != "1" {
		return
	}
	lock, err := acquireBackupActivityLock(os.Getenv("MSBOOST_BACKUP_LOCK_DIR"), false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, lock.Identity)
	var wait [1]byte
	_, _ = os.Stdin.Read(wait[:])
	_ = lock.Close()
	os.Exit(0)
}

func TestBackupActivityLockLinuxCrossProcessDeathReleasesOriginalInode(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireBackupActivityLock(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	identity, device, inode := lock.Identity, lock.Device, lock.Inode
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackupActivityLockProcessHelper$")
	child.Env = append(os.Environ(), "MSBOOST_BACKUP_LOCK_CHILD=1", "MSBOOST_BACKUP_LOCK_DIR="+dir)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != identity {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatalf("child did not acquire original lock: %q %v stderr=%s", line, err, stderr.String())
	}
	if second, err := acquireBackupActivityLock(dir, false); !errors.Is(err, errBackupActivityLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("another process's live lock was not exclusive: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	after, err := acquireBackupActivityLock(dir, false)
	if err != nil {
		t.Fatalf("process death left a stale sentinel lock: %v", err)
	}
	defer after.Close()
	if after.Identity != identity || after.Device != device || after.Inode != inode {
		t.Fatal("crash recovery did not lock the original persistent inode")
	}
}

func TestBackupActivityLockLinuxRejectsUnsafePathsAndOwnership(t *testing.T) {
	t.Run("symlink parent", func(t *testing.T) {
		dir := t.TempDir()
		link := filepath.Join(t.TempDir(), "linked-data")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if lock, err := acquireBackupActivityLock(link, true); err == nil {
			_ = lock.Close()
			t.Fatal("accepted symlinked data directory")
		}
		if _, err := os.Stat(filepath.Join(dir, backupActivityLockName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("created lock through a symlinked parent")
		}
	})
	t.Run("writable data directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0777); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0700)
		if lock, err := acquireBackupActivityLock(dir, true); err == nil {
			_ = lock.Close()
			t.Fatal("accepted group/world-writable data directory")
		}
	})
	for _, mode := range []os.FileMode{0644, 0660, 0400} {
		t.Run(fmt.Sprintf("mode-%o", mode), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, backupActivityLockName)
			if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if lock, err := acquireBackupActivityLock(dir, false); err == nil {
				_ = lock.Close()
				t.Fatal("accepted unsafe file permissions")
			}
		})
	}
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			link := os.Link
			if kind == "symlink" {
				link = os.Symlink
			}
			if err := link(target, filepath.Join(dir, backupActivityLockName)); err != nil {
				t.Fatal(err)
			}
			for _, create := range []bool{false, true} {
				if lock, err := acquireBackupActivityLock(dir, create); err == nil {
					_ = lock.Close()
					t.Fatalf("create=%v accepted %s lock", create, kind)
				}
			}
		})
	}
	t.Run("mismatched file owner", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("changing ownership requires isolated Linux root test")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, backupActivityLockName)
		if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 10001, 10001); err != nil {
			t.Fatal(err)
		}
		if lock, err := acquireBackupActivityLock(dir, false); err == nil {
			_ = lock.Close()
			t.Fatal("accepted lock owned by a different UID from dataDir")
		}
	})
}

func TestBackupActivityLockLinuxReadOnlyAndCopiedNonce(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	first, err := acquireBackupActivityLock(dir, true)
	if err != nil {
		t.Fatalf("official 0755 application data directory rejected: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	readOnly, err := acquireBackupActivityLock(dir, false)
	if err != nil {
		t.Fatalf("read-only acquisition attempted a directory write: %v", err)
	}
	_ = readOnly.Close()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, backupActivityLockName)
	// Preserve the old inode under a test-only name so inode reuse cannot make
	// the test nondeterministic. Production code never renames or replaces it.
	if err := os.Rename(path, filepath.Join(dir, "old-test-inode")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(first.Identity+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement, err := acquireBackupActivityLock(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if replacement.Identity != first.Identity || (replacement.Device == first.Device && replacement.Inode == first.Inode) {
		t.Fatal("copied nonce did not retain a distinguishable replacement inode identity")
	}
}

func TestBackupActivityLockLinuxValidateDetectsReplacementAndPermissionChanges(t *testing.T) {
	for _, mutation := range []string{"replace inode", "replace directory", "file permissions", "directory permissions", "hardlink"} {
		t.Run(mutation, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "data")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			lock, err := acquireBackupActivityLock(dir, true)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			path := filepath.Join(dir, backupActivityLockName)
			switch mutation {
			case "replace inode":
				if err := os.Rename(path, filepath.Join(dir, "old-test-inode")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(lock.Identity+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "replace directory":
				if err := os.Rename(dir, filepath.Join(parent, "old-data")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(lock.Identity+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "file permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory permissions":
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(dir, 0700)
			case "hardlink":
				if err := os.Link(path, filepath.Join(dir, "second-link")); err != nil {
					t.Fatal(err)
				}
			}
			if err := lock.Validate(); err == nil {
				t.Fatalf("live validation missed %s", mutation)
			}
		})
	}
}

package relayruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestV2StateDirectoryLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production flock requires Linux; non-Linux helper is not a lock")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockV2StateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	checkChild := func(expect string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestV2StateDirectoryLockChild$")
		cmd.Env = append(os.Environ(), "MSBOOST_V2_LOCK_TEST_DIR="+dir, "MSBOOST_V2_LOCK_TEST_EXPECT="+expect)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("cross-process lock check (%s): %v: %s", expect, err, output)
		}
	}
	checkChild("locked")
	unlock()
	checkChild("available")
	info, err := os.Lstat(filepath.Join(dir, "relay-v2.lock"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("private stable lock file missing: %v", err)
	}
}

func TestV2StateDirectoryLockChild(t *testing.T) {
	dir := os.Getenv("MSBOOST_V2_LOCK_TEST_DIR")
	if runtime.GOOS != "linux" || dir == "" {
		t.Skip("called only by Linux cross-process lock test")
	}
	unlock, err := lockV2StateDir(dir)
	if os.Getenv("MSBOOST_V2_LOCK_TEST_EXPECT") == "locked" {
		if err == nil {
			unlock()
			t.Fatal("second Agent acquired the same state directory")
		}
		return
	}
	if err != nil {
		t.Fatalf("exited Agent retained kernel lock: %v", err)
	}
	unlock()
}

func TestV2StateDirectoryLockRejectsUntrustedFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production lock file ownership checks require Linux")
	}
	for _, kind := range []string{"symlink", "hardlink", "public", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			lockPath := filepath.Join(dir, "relay-v2.lock")
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, nil, 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, lockPath)
			case "hardlink":
				err = os.Link(target, lockPath)
			case "public":
				err = os.WriteFile(lockPath, nil, 0600)
				if err == nil {
					err = os.Chmod(lockPath, 0644)
				}
			case "directory":
				err = os.Mkdir(lockPath, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if unlock, err := lockV2StateDir(dir); err == nil {
				unlock()
				t.Fatal("accepted untrusted " + kind + " lock file")
			}
		})
	}
}

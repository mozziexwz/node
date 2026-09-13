package control

import (
	"context"
	"testing"
)

func TestBackupSFTPOwnershipCheckedBeforePrivateBytes(t *testing.T) {
	for _, scenario := range []string{"root-safe", "root-other-parent", "root-other-leaf", "user-safe", "user-other-parent", "user-other-leaf"} {
		t.Run(scenario, func(t *testing.T) {
			b, server, target := backupUploadFixture(t)
			uid := uint32(0)
			if scenario[:4] == "user" {
				uid = 1001
				target.User = "backup"
				server.allowedUser.Store("backup")
			}
			target.Path = "/home/backup/data"
			sf := server.connect(t)
			for _, dir := range []string{"/home", "/home/backup", "/home/backup/data"} {
				if err := sf.Mkdir(dir); err != nil {
					t.Fatal(err)
				}
				if err := sf.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			server.files.mu.Lock()
			server.files.createUID = uid
			server.files.owners["/home/backup"] = uid
			server.files.owners[target.Path] = uid
			if scenario == "root-other-parent" || scenario == "user-other-parent" {
				server.files.owners["/home/backup"] = 1002
			}
			if scenario == "root-other-leaf" || scenario == "user-other-leaf" {
				server.files.owners[target.Path] = 1002
			}
			server.files.mu.Unlock()
			raw := []byte("private fixture payload")
			err := b.upload(context.Background(), target, "owner-check", raw)
			ok := scenario == "root-safe" || scenario == "user-safe"
			if (err == nil) != ok {
				t.Fatalf("unexpected owner verdict: %v", err)
			}
			server.files.mu.Lock()
			writes := server.files.writes
			unsafe := server.files.unsafeWrite
			server.files.mu.Unlock()
			if !ok && writes != 0 {
				t.Fatal("private data was sent through other-account-owned path")
			}
			if ok && writes != len(raw) || unsafe {
				t.Fatal("valid self-owned upload did not remain private")
			}
		})
	}
}

package disaster

import (
	"context"
	"path"
	"testing"
)

func TestDisasterSFTPOwnershipCheckedBeforePrivateBytes(t *testing.T) {
	archive := remoteBundleFixture(t)
	for _, scenario := range []string{"root-safe", "root-other-parent", "root-other-leaf", "user-safe", "user-other-parent", "user-other-leaf"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRemoteFixture(t)
			config := remoteTestConfig(t, f)
			uid := uint32(0)
			if scenario[:4] == "user" {
				uid = 1001
				config.RemoteUser = "backup"
				f.allowedUser.Store("backup")
			}
			config.RemoteDir = "/home/backup/data"
			sf := f.connect(t)
			for _, dir := range []string{"/home", "/home/backup", config.RemoteDir} {
				if err := sf.Mkdir(dir); err != nil {
					t.Fatal(err)
				}
				if err := sf.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			f.fs.mu.Lock()
			f.fs.createUID = uid
			f.fs.owners["/home/backup"] = uid
			f.fs.owners[config.RemoteDir] = uid
			if scenario == "root-other-parent" || scenario == "user-other-parent" {
				f.fs.owners["/home/backup"] = 1002
			}
			if scenario == "root-other-leaf" || scenario == "user-other-leaf" {
				f.fs.owners[config.RemoteDir] = 1002
			}
			f.fs.mu.Unlock()
			err := uploadWithDial(context.Background(), config, archive, f.dial(t))
			ok := scenario == "root-safe" || scenario == "user-safe"
			if (err == nil) != ok {
				t.Fatalf("unexpected owner verdict: %v", err)
			}
			f.fs.mu.Lock()
			writes, unsafe := f.fs.writes, f.fs.unsafeWrite
			f.fs.mu.Unlock()
			if !ok && writes != 0 {
				t.Fatal("secret bundle bytes sent to an unsafe owner path")
			}
			if ok && writes == 0 || unsafe {
				t.Fatal("valid self-owned upload did not remain private")
			}
		})
	}
}

func TestDisasterRemoteRetentionRejectsForeignDirectoryOrCandidateOwner(t *testing.T) {
	for _, scenario := range []string{"parent", "leaf", "candidate"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRemoteFixture(t)
			config := remoteTestConfig(t, f)
			config.PruneRemote = true
			sf := f.connect(t)
			uid := uint32(0)
			if err := privateRemoteDirectory(sf, config.RemoteDir, &uid); err != nil {
				t.Fatal(err)
			}
			var oldest string
			for i, age := range []int{1, 2, 60} {
				oldest = seedRemoteBundle(t, sf, config.RemoteDir, remoteAgedBundle(t, i+1, age))
			}
			f.fs.mu.Lock()
			switch scenario {
			case "parent":
				f.fs.owners[path.Dir(config.RemoteDir)] = 1002
			case "leaf":
				f.fs.owners[config.RemoteDir] = 1002
			case "candidate":
				f.fs.owners[oldest] = 1002
			}
			f.fs.mu.Unlock()
			if err := pruneRemote(sf, config, uid); err == nil {
				t.Fatal("foreign-owned retention accepted")
			}
			f.fs.mu.Lock()
			defer f.fs.mu.Unlock()
			if f.fs.removes != 0 {
				t.Fatal("retention deleted through foreign ownership")
			}
		})
	}
}

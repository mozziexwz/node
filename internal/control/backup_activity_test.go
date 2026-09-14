package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func backupActivityTarget(t *testing.T, a *App) BackupTarget {
	t.Helper()
	_, signer := backupTestKey(t)
	sealed, err := a.Seal([]byte("isolated-backup-activity-password"))
	if err != nil {
		t.Fatal(err)
	}
	target := BackupTarget{ID: "activity-target", Name: "isolated target", Host: "8.8.8.8", Port: 22, User: "root", Path: "/root/msboost-backup", Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), AuthMode: "password", SealedPassword: sealed, Enabled: true}
	if err := a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "backup_targets", target.ID, target); err != nil {
			return err
		}
		return SaveDoc(s, "backup_plans", "default", BackupPlan{Targets: []string{target.ID}})
	}); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestBackupActivityCoversLocalFileAndRemoteWork(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	target := backupActivityTarget(t, a)
	dialed := false
	b.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		if err := a.Store.View(func(s *State) error {
			operations := ListDocs[BackupOperation](s, "backup_operations")
			if len(operations) != 1 || operations[0].Kind != "site-backup" || operations[0].StartedAt <= 0 {
				t.Errorf("missing durable admission during external work: %+v", operations)
			}
			if len(s.Docs["backups"]) != 0 {
				t.Error("backup result was committed before external work finished")
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
		files, err := filepath.Glob(filepath.Join(a.Config.DataDir, "backups", "*.msb"))
		if err != nil || len(files) != 1 {
			t.Errorf("local file must be complete before upload: %v %v", files, err)
		} else {
			raw, err := os.ReadFile(files[0])
			if err != nil {
				t.Error(err)
			} else if snapshot, err := b.unpack(raw); err != nil {
				t.Error(err)
			} else if backupOperationPending(snapshot) {
				t.Error("snapshot included its own unfinished backup activity")
			}
		}
		return nil, errors.New("isolated remote failure")
	}
	record, err := b.run(context.Background())
	if err != nil || !dialed || record.Status != "partial" || record.Targets[target.ID] != "failed" || record.Error == "" {
		t.Fatalf("remote failure must remain visible in result: %+v %v dialed=%v", record, err, dialed)
	}
	if err := a.Store.View(func(s *State) error {
		if backupOperationPending(s) {
			t.Error("finished external work retained its activity")
		}
		stored, exists := LoadDoc[BackupRecord](s, "backups", record.ID)
		if !exists || stored.Status != "partial" || stored.Targets[target.ID] != "failed" {
			t.Errorf("partial result not saved with activity removal: %+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupActivityLocalSuccessAndWriteFailure(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		name := "success"
		if failWrite {
			name = "write-failure"
		}
		t.Run(name, func(t *testing.T) {
			a, _, _ := commerceTestApp(t)
			b := NewBackupService(a)
			if failWrite {
				file := filepath.Join(a.Config.DataDir, "backups")
				if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			record, err := b.run(context.Background())
			if (err != nil) != failWrite {
				t.Fatalf("unexpected backup result: %+v %v", record, err)
			}
			if err := a.Store.View(func(s *State) error {
				if backupOperationPending(s) {
					t.Error("completed local work retained its marker")
				}
				stored, exists := LoadDoc[BackupRecord](s, "backups", record.ID)
				want := "verified"
				if failWrite {
					want = "failed"
				}
				if !exists || stored.Status != want || failWrite && stored.Error == "" {
					t.Errorf("local outcome not persisted: %+v", stored)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupActivityAdmissionBlocksBeforePackingOrExternalWork(t *testing.T) {
	for _, kind := range []string{"pause-gate", "crash-marker", "malformed-marker"} {
		t.Run(kind, func(t *testing.T) {
			a, _, _ := commerceTestApp(t)
			b := NewBackupService(a)
			backupActivityTarget(t, a)
			if err := a.Store.Update(func(s *State) error {
				switch kind {
				case "pause-gate":
					return SaveDoc(s, "backup_pause", "default", map[string]any{"invalid": "still locks"})
				case "crash-marker":
					return beginBackupOperation(s, BackupOperation{ID: ID(), Kind: "site-backup", StartedAt: time.Now().Add(-365 * 24 * time.Hour).UnixMilli()})
				default:
					return SaveDoc(s, "backup_operations", "damaged", nil)
				}
			}); err != nil {
				t.Fatal(err)
			}
			// A nil cipher would panic if pack were reached. Admission must reject
			// before even producing a snapshot, not merely before its final save.
			a.aead = nil
			b.dialContext = func(context.Context, string, string) (net.Conn, error) {
				t.Error("rejected backup performed remote work")
				return nil, errors.New("unexpected dial")
			}
			record, err := b.run(context.Background())
			if err == nil || record.ID != "" || kind == "pause-gate" && !errors.Is(err, ErrBackupPauseActive) {
				t.Fatalf("backup was not rejected safely: %+v %v", record, err)
			}
			if _, err := os.Stat(filepath.Join(a.Config.DataDir, "backups")); !os.IsNotExist(err) {
				t.Fatalf("rejected backup created local files: %v", err)
			}
			if _, err := os.Stat(filepath.Join(a.Config.DataDir, backupActivityLockName)); !os.IsNotExist(err) {
				t.Fatalf("rejected backup created a lock file behind the pause gate: %v", err)
			}
		})
	}
}

func TestBackupActivityUnconfirmedFinalCommitIsNotSuccess(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	backupActivityTarget(t, a)
	observer, err := openStore(a.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	b.dialContext = func(context.Context, string, string) (net.Conn, error) {
		// External work has finished, but the final database transaction cannot
		// commit. Leave the intent for an independent process to reconcile.
		if err := a.Store.Close(); err != nil {
			t.Error(err)
		}
		return nil, errors.New("isolated remote failure")
	}
	_, err = b.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "活动标记未确认提交") {
		t.Fatalf("unconfirmed finalization reported success: %v", err)
	}
	if err := observer.View(func(s *State) error {
		if !backupOperationPending(s) || len(s.Docs["backups"]) != 0 {
			t.Error("failed final transaction silently cleared intent or saved success")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

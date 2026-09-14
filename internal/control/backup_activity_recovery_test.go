package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func backupActivityRecoveryFixture(t *testing.T) (*Store, string, BackupOperation, string) {
	t.Helper()
	directory := t.TempDir()
	store, err := openStore(Config{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lock, err := acquireBackupActivityLock(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	operation := BackupOperation{ID: ID(), Kind: "site-backup", StartedAt: time.Now().Add(-24 * time.Hour).UnixMilli(), LockIdentity: lock.Identity, LockDevice: lock.Device, LockInode: lock.Inode}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	if err = store.Update(func(s *State) error {
		if err := beginBackupOperation(s, operation); err != nil {
			return err
		}
		fingerprint = backupActivityFingerprint(s.Docs["backup_operations"][operation.ID])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store, directory, operation, fingerprint
}

func backupActivityStateBytes(t *testing.T, store *Store) []byte {
	t.Helper()
	var raw []byte
	if err := store.View(func(s *State) error { var err error; raw, err = json.Marshal(s); return err }); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBackupActivityInspectIsReadOnlyAndBindsExactMarker(t *testing.T) {
	store, directory, operation, fingerprint := backupActivityRecoveryFixture(t)
	before := backupActivityStateBytes(t, store)
	result, err := inspectBackupActivity(store, directory)
	if err != nil || !result.Pending || !result.Eligible || result.OperationID != operation.ID || result.Fingerprint != fingerprint {
		t.Fatal(result, err)
	}
	if !bytes.Equal(before, backupActivityStateBytes(t, store)) {
		t.Fatal("inspection changed state")
	}
	var output bytes.Buffer
	if err = writeBackupActivityInspection(&output, result, true); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(output.String(), "\n")
	if len(lines) != 7 || lines[0] != "MSBOOST_BACKUP_ACTIVITY_INSPECT_V1" || lines[1] != "1" || lines[2] != "1" || lines[3] != operation.ID || lines[4] != fingerprint || lines[6] != "" {
		t.Fatal("bad fixed frame", output.String())
	}
	output.Reset()
	if err = writeBackupActivityInspection(&output, result, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), operation.LockIdentity) || strings.Contains(output.String(), "lockIdentity") {
		t.Fatal("private lock identity leaked")
	}
}

func TestBackupActivityLiveOwnerCannotBeReconciled(t *testing.T) {
	store, directory, operation, fingerprint := backupActivityRecoveryFixture(t)
	owner, err := acquireBackupActivityLock(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	before := backupActivityStateBytes(t, store)
	result, err := inspectBackupActivity(store, directory)
	if err != nil || !result.Pending || result.Eligible {
		t.Fatal(result, err)
	}
	if _, err = reconcileBackupActivity(store, directory, operation.ID, fingerprint, time.Now().UnixMilli()); err == nil {
		t.Fatal("live backup was reconciled")
	}
	if !bytes.Equal(before, backupActivityStateBytes(t, store)) {
		t.Fatal("live marker changed")
	}
}

func TestBackupActivityReconcileOnlyMovesMarkerAndKeepsUnknown(t *testing.T) {
	store, directory, operation, fingerprint := backupActivityRecoveryFixture(t)
	if err := store.Update(func(s *State) error {
		s.Users["member"] = &User{ID: "member", BalanceCents: 1234, TrafficUsed: 5678}
		s.Settings["maintenance"] = true
		return SaveDoc(s, "relay_v2_control", "default", RelayV2Control{Epoch: "unchanged", RecoveryRequired: true})
	}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, operation.ID+".candidate")
	if err := os.WriteFile(file, []byte("unverified candidate stays intact"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := reconcileBackupActivity(store, directory, operation.ID, fingerprint, time.Now().UnixMilli())
	if err != nil || result.Status != "interrupted_unknown" || result.Operation != operation {
		t.Fatal(result, err)
	}
	if err = store.View(func(s *State) error {
		if backupOperationPending(s) || len(s.Docs["backups"]) != 0 || len(s.Docs["backup_operation_reconciliations"]) != 1 {
			t.Error("unknown operation became a successful backup")
		}
		if s.Users["member"].BalanceCents != 1234 || s.Users["member"].TrafficUsed != 5678 || !boolSetting(s, "maintenance") || !restoredRelayRecoveryRequired(s) {
			t.Error("unrelated financial/recovery state changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != "unverified candidate stays intact" {
		t.Fatal("candidate changed", err)
	}
	second, err := reconcileBackupActivity(store, directory, operation.ID, fingerprint, time.Now().UnixMilli()+5000)
	if err != nil || second != result {
		t.Fatal("uncertain result retry changed audit", second, err)
	}
	var output bytes.Buffer
	if err = writeBackupActivityReconciliation(&output, result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), operation.LockIdentity) || strings.Contains(output.String(), "verified") {
		t.Fatal("misleading or secret output", output.String())
	}
}

func TestBackupActivityStaleInspectionAndGateCannotBeBypassed(t *testing.T) {
	for _, kind := range []string{"changed-marker", "different-id", "wrong-hash", "multiple", "gate", "legacy", "different-lock", "different-inode", "different-device", "prior-audit"} {
		t.Run(kind, func(t *testing.T) {
			store, directory, operation, fingerprint := backupActivityRecoveryFixture(t)
			id := operation.ID
			if err := store.Update(func(s *State) error {
				switch kind {
				case "changed-marker":
					operation.StartedAt++
					return SaveDoc(s, "backup_operations", operation.ID, operation)
				case "different-id":
					id = "wrong"
				case "wrong-hash":
					fingerprint = strings.Repeat("aa", 32)
				case "multiple":
					return SaveDoc(s, "backup_operations", "another", operation)
				case "gate":
					return SaveDoc(s, backupPauseCollection, "default", map[string]bool{"exists": true})
				case "legacy":
					operation.LockIdentity = ""
					return SaveDoc(s, "backup_operations", operation.ID, operation)
				case "different-lock":
					operation.LockIdentity = strings.Repeat("ab", 32)
					return SaveDoc(s, "backup_operations", operation.ID, operation)
				case "different-inode":
					operation.LockInode = "different"
					return SaveDoc(s, "backup_operations", operation.ID, operation)
				case "different-device":
					operation.LockDevice = "different"
					return SaveDoc(s, "backup_operations", operation.ID, operation)
				case "prior-audit":
					return SaveDoc(s, "backup_operation_reconciliations", operation.ID, map[string]string{"keep": "existing audit"})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := backupActivityStateBytes(t, store)
			if _, err := reconcileBackupActivity(store, directory, id, fingerprint, time.Now().UnixMilli()); err == nil {
				t.Fatal("unsafe reconciliation allowed")
			}
			if !bytes.Equal(before, backupActivityStateBytes(t, store)) {
				t.Fatal("failed review mutated state")
			}
		})
	}
}

func TestBackupActivityMissingOrCorruptLockIsNeverReplaced(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			store, directory, operation, fingerprint := backupActivityRecoveryFixture(t)
			file := filepath.Join(directory, backupActivityLockName)
			if kind == "missing" {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(file, []byte("broken"), 0600); err != nil {
				t.Fatal(err)
			}
			before := backupActivityStateBytes(t, store)
			result, err := inspectBackupActivity(store, directory)
			if err != nil || result.Eligible {
				t.Fatal(result, err)
			}
			if _, err = reconcileBackupActivity(store, directory, operation.ID, fingerprint, time.Now().UnixMilli()); err == nil {
				t.Fatal("bad lock accepted")
			}
			if kind == "missing" {
				if _, err := os.Stat(file); !os.IsNotExist(err) {
					t.Fatal("replacement lock created")
				}
			} else if raw, err := os.ReadFile(file); err != nil || string(raw) != "broken" {
				t.Fatal("corrupt lock repaired")
			}
			if !bytes.Equal(before, backupActivityStateBytes(t, store)) {
				t.Fatal("bad lock cleared marker")
			}
		})
	}
}

type failingBackupActivityWriter struct{}

func (failingBackupActivityWriter) Write([]byte) (int, error) {
	return 0, errors.New("simulated broken output")
}

func TestBackupActivityCommitAndOutputFailureRemainReviewable(t *testing.T) {
	store, directory, operation, fingerprint := backupActivityRecoveryFixture(t)
	if _, err := store.db.Exec(`CREATE TRIGGER reject_review BEFORE UPDATE ON control_state BEGIN SELECT RAISE(ABORT,'simulated'); END`); err != nil {
		t.Fatal(err)
	}
	before := backupActivityStateBytes(t, store)
	if _, err := reconcileBackupActivity(store, directory, operation.ID, fingerprint, time.Now().UnixMilli()); err == nil {
		t.Fatal("commit failure reported success")
	}
	if !bytes.Equal(before, backupActivityStateBytes(t, store)) {
		t.Fatal("failed transaction changed record")
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_review`); err != nil {
		t.Fatal(err)
	}
	result, err := reconcileBackupActivity(store, directory, operation.ID, fingerprint, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err = writeBackupActivityReconciliation(failingBackupActivityWriter{}, result); err == nil {
		t.Fatal("output failure reported success")
	}
	if retried, err := reconcileBackupActivity(store, directory, operation.ID, fingerprint, time.Now().UnixMilli()); err != nil || retried != result {
		t.Fatal("unknown output could not retry", err)
	}
	lock, err := acquireBackupActivityLock(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	newOperation := BackupOperation{ID: ID(), Kind: "site-backup", StartedAt: time.Now().UnixMilli(), LockIdentity: lock.Identity, LockDevice: lock.Device, LockInode: lock.Inode}
	_ = lock.Close()
	if err = store.Update(func(s *State) error { return beginBackupOperation(s, newOperation) }); err != nil {
		t.Fatal(err)
	}
	before = backupActivityStateBytes(t, store)
	if _, err = reconcileBackupActivity(store, directory, operation.ID, fingerprint, time.Now().UnixMilli()); err == nil {
		t.Fatal("old retry ignored new activity")
	}
	if !bytes.Equal(before, backupActivityStateBytes(t, store)) {
		t.Fatal("old retry changed new activity")
	}
}

func TestBackupActivityConfirmationRequiresExactBoundedFrame(t *testing.T) {
	id, fp := "review-safe-ID", strings.Repeat("ab", 32)
	valid := id + "\n" + fp + "\nRECONCILE_BACKUP " + id + "\n"
	for _, invalid := range []string{"", strings.TrimSuffix(valid, "\n"), valid + "extra\n", strings.ReplaceAll(valid, "\n", "\r\n"), strings.Replace(valid, "RECONCILE_BACKUP "+id, "YES", 1), strings.ReplaceAll(valid, id, "../../wrong"), strings.Replace(valid, fp, strings.ToUpper(fp), 1), strings.Repeat("x", 385)} {
		if _, _, err := readBackupActivityConfirmation(strings.NewReader(invalid)); err == nil {
			t.Fatal("invalid confirmation accepted")
		}
	}
	if gotID, gotFP, err := readBackupActivityConfirmation(strings.NewReader(valid)); err != nil || gotID != id || gotFP != fp {
		t.Fatal(err)
	}
}

func TestBackupActivityPanicRetainsUnknownAndReleasesKernelLock(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	backupActivityTarget(t, a)
	b.dialContext = func(context.Context, string, string) (net.Conn, error) { panic("isolated interrupted upload") }
	panicked := false
	func() { defer func() { panicked = recover() != nil }(); _, _ = b.run(context.Background()) }()
	if !panicked {
		t.Fatal("unexpected panic handling")
	}
	result, err := inspectBackupActivity(a.Store, a.Config.DataDir)
	if err != nil || !result.Pending || !result.Eligible {
		t.Fatal("crashed owner not reviewable", result, err)
	}
	if err = a.Store.View(func(s *State) error {
		if len(s.Docs["backups"]) != 0 || len(s.Docs["backup_operations"]) != 1 {
			t.Error("panic became verified or lost intent")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(a.Config.DataDir, "backups", result.OperationID+".msb")); err != nil {
		t.Fatal("new operation identity cannot identify its candidate", err)
	}
}

func TestBackupActivityEmptyInspectionCreatesNothing(t *testing.T) {
	directory := t.TempDir()
	store, err := openStore(Config{DataDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	result, err := inspectBackupActivity(store, directory)
	if err != nil || result.Pending || result.Eligible {
		t.Fatal(result, err)
	}
	if _, err := os.Stat(filepath.Join(directory, backupActivityLockName)); !os.IsNotExist(err) {
		t.Fatal("empty inspection created a lock")
	}
}

func TestBackupActivityChangedLockCannotFinalizeOrReconcile(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	backupActivityTarget(t, a)
	b.dialContext = func(context.Context, string, string) (net.Conn, error) {
		if err := os.WriteFile(filepath.Join(a.Config.DataDir, backupActivityLockName), []byte(strings.Repeat("00", 32)+"\n"), 0600); err != nil {
			t.Error(err)
		}
		return nil, errors.New("isolated upload failure")
	}
	if _, err := b.run(context.Background()); err == nil {
		t.Fatal("changed lock finalized activity")
	}
	result, err := inspectBackupActivity(a.Store, a.Config.DataDir)
	if err != nil || !result.Pending || result.Eligible {
		t.Fatal("changed lock authorized review", result, err)
	}
	if err = a.Store.View(func(s *State) error {
		if len(s.Docs["backups"]) != 0 || len(s.Docs["backup_operations"]) != 1 {
			t.Error("changed lock lost marker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

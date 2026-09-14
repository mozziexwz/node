package control

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

// This is a review of local backup orchestration only. It does not clear a
// disaster pause gate, prove remote upload completion or settle financial data.
type BackupActivityInspection struct {
	Pending     bool   `json:"pending"`
	Eligible    bool   `json:"eligible"`
	OperationID string `json:"operationId,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	StartedAt   int64  `json:"startedAt,omitempty"`
	Message     string `json:"message"`
}

type BackupActivityReconciliation struct {
	OperationID  string          `json:"operationId"`
	Fingerprint  string          `json:"fingerprint"`
	ReconciledAt int64           `json:"reconciledAt"`
	Status       string          `json:"status"`
	Operation    BackupOperation `json:"operation"`
}

var errBackupActivityReview = errors.New("备份活动证据不一致或仍在执行，未核销；请重新检查原进程、原数据卷和当前记录")

func backupActivityFingerprint(raw json.RawMessage) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func backupActivityCurrent(s *State) (BackupActivityInspection, BackupOperation) {
	var operation BackupOperation
	result := BackupActivityInspection{Pending: backupOperationPending(s), Message: "没有待核销的网页备份活动；没有执行任何修改。"}
	if !result.Pending {
		return result, operation
	}
	result.Message = "活动记录不完整、属于旧协议或有多个记录，不能自动核销；不会按时间判断结束。"
	if len(s.Docs["backup_operations"]) != 1 {
		return result, operation
	}
	for id, raw := range s.Docs["backup_operations"] {
		if !validBackupID(id) || !backupPauseDecode(raw, &operation) || operation.ID != id || operation.Kind != "site-backup" || operation.StartedAt <= 0 {
			return result, BackupOperation{}
		}
		result.OperationID, result.Fingerprint, result.StartedAt = id, backupActivityFingerprint(raw), operation.StartedAt
	}
	return result, operation
}

func backupActivityLockMatches(operation BackupOperation, lock *backupActivityLock) bool {
	return lock != nil && backupPauseValidToken(operation.LockIdentity) && operation.LockIdentity == lock.Identity && operation.LockDevice != "" && operation.LockDevice == lock.Device && operation.LockInode != "" && operation.LockInode == lock.Inode
}

func inspectBackupActivity(store *Store, dataDir string) (result BackupActivityInspection, err error) {
	var operation BackupOperation
	if err = store.View(func(s *State) error {
		result, operation = backupActivityCurrent(s)
		if backupPauseGatePresent(s) {
			result.Message = "整站备份门禁尚未解除；本入口不能替代原令牌解锁。"
			operation = BackupOperation{}
		}
		return nil
	}); err != nil {
		return result, err
	}
	if !result.Pending || !backupPauseValidToken(operation.LockIdentity) {
		return result, nil
	}
	lock, lockErr := acquireBackupActivityLock(dataDir, false)
	if lockErr != nil {
		result.Message = "原备份仍在运行，或原数据卷/锁证据不可验证；未获准核销，不会停止服务。"
		return result, nil
	}
	defer func() {
		if lock.Close() != nil {
			result.Eligible = false
			err = errBackupActivityReview
		}
	}()
	// Re-read after locking: the first view might have raced a normal finish.
	err = store.View(func(s *State) error {
		if lock.Validate() != nil {
			return errBackupActivityReview
		}
		result, operation = backupActivityCurrent(s)
		if backupPauseGatePresent(s) {
			result.Message = "整站备份门禁尚未解除；必须用原令牌处理。"
			return nil
		}
		if !backupActivityLockMatches(operation, lock) {
			return nil
		}
		result.Eligible = true
		result.Message = "原锁可独占且活动记录匹配；可在确认精确记录后核销为结果未知。此检查不证明本机副本或远端上传成功。"
		return nil
	})
	return result, err
}

func readBackupActivityConfirmation(input io.Reader) (id, fingerprint string, err error) {
	raw, err := io.ReadAll(io.LimitReader(input, 385))
	if err != nil || len(raw) > 384 || bytes.ContainsAny(raw, "\x00\r") {
		return "", "", errBackupActivityReview
	}
	parts := strings.Split(string(raw), "\n")
	if len(parts) != 4 || parts[3] != "" || !validBackupID(parts[0]) || !backupPauseValidToken(parts[1]) || parts[2] != "RECONCILE_BACKUP "+parts[0] {
		return "", "", errBackupActivityReview
	}
	return parts[0], parts[1], nil
}

func reconcileBackupActivity(store *Store, dataDir, id, fingerprint string, now int64) (result BackupActivityReconciliation, err error) {
	if !validBackupID(id) || !backupPauseValidToken(fingerprint) || now <= 0 {
		return result, errBackupActivityReview
	}
	lock, err := acquireBackupActivityLock(dataDir, false)
	if err != nil {
		return result, errBackupActivityReview
	}
	defer func() {
		if lock.Close() != nil {
			err = errors.Join(err, errBackupActivityReview)
		}
	}()
	err = store.Update(func(s *State) error {
		if lock.Validate() != nil {
			return errBackupActivityReview
		}
		// Idempotent retry is permitted only for this exact audited operation,
		// and never when a new/unrelated operation has appeared meanwhile.
		if !backupOperationPending(s) {
			var prior BackupActivityReconciliation
			if !backupPauseDecode(s.Docs["backup_operation_reconciliations"][id], &prior) || prior.OperationID != id || prior.Operation.ID != id || prior.Operation.Kind != "site-backup" || prior.Operation.StartedAt <= 0 || prior.Fingerprint != fingerprint || prior.Status != "interrupted_unknown" || prior.ReconciledAt <= 0 || !backupActivityLockMatches(prior.Operation, lock) {
				return errBackupActivityReview
			}
			result = prior
			return nil
		}
		inspection, operation := backupActivityCurrent(s)
		if inspection.OperationID != id || inspection.Fingerprint != fingerprint || !backupActivityLockMatches(operation, lock) {
			return errBackupActivityReview
		}
		// Do not overwrite an earlier review entry or a colliding result.
		if _, exists := s.Docs["backup_operation_reconciliations"][id]; exists {
			return errBackupActivityReview
		}
		result = BackupActivityReconciliation{OperationID: id, Fingerprint: fingerprint, ReconciledAt: now, Status: "interrupted_unknown", Operation: operation}
		if err := SaveDoc(s, "backup_operation_reconciliations", id, result); err != nil {
			return err
		}
		DeleteDoc(s, "backup_operations", id)
		contentAudit(s, "local-root", "backup.activity.reconcile_unknown", id)
		return nil
	})
	return result, err
}

// This command is intentionally local-only with no HTTP mutation equivalent.
// It never opens a missing SQLite file or initializes/migrates an existing DB.
func RunLocalBackupActivity(databaseURL, dataDir string, args []string, input io.Reader, output io.Writer) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("异常网页备份核销仅允许目标 Linux 服务器 root 执行")
	}
	if len(args) != 1 || args[0] != "inspect" && args[0] != "inspect-lines" && args[0] != "reconcile" {
		return errors.New("backup-activity 仅接受 inspect、inspect-lines 或 reconcile；确认内容只能从 stdin 输入")
	}
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return errors.New("异常备份核销要求现有 PostgreSQL 数据库")
	}
	if dataDir == "" {
		return errors.New("必须选择原应用数据目录，不会创建替代目录或锁文件")
	}
	var id, fingerprint string
	var err error
	if args[0] == "reconcile" {
		id, fingerprint, err = readBackupActivityConfirmation(input)
		if err != nil {
			return err
		}
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("无法打开原站点数据库")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, dialect: "postgres"}
	if args[0] == "reconcile" {
		result, err := reconcileBackupActivity(store, dataDir, id, fingerprint, time.Now().UnixMilli())
		if err != nil {
			return errors.New("异常备份核销未确认提交；未创建数据库或删除文件，请用相同记录重新核对。整站门禁或中转恢复状态不会被此入口解除")
		}
		// Do not expose lock identity or the private on-disk marker details.
		return writeBackupActivityReconciliation(output, result)
	}
	result, err := inspectBackupActivity(store, dataDir)
	if err != nil {
		return errors.New("无法核对原备份活动；未修改数据库、锁文件或任何服务")
	}
	return writeBackupActivityInspection(output, result, args[0] == "inspect-lines")
}

func writeBackupActivityReconciliation(output io.Writer, result BackupActivityReconciliation) error {
	if json.NewEncoder(output).Encode(map[string]any{"operationId": result.OperationID, "fingerprint": result.Fingerprint, "status": result.Status, "reconciledAt": result.ReconciledAt, "message": "已将此网页备份活动登记为结果未知；文件和远端均未删除，未宣称备份成功。"}) != nil {
		return errors.New("核销结果输出失败，提交状态未确认；保留原 ID 和指纹重新核对")
	}
	return nil
}

func writeBackupActivityInspection(output io.Writer, result BackupActivityInspection, lines bool) error {
	var err error
	if lines {
		id, fingerprint := "-", "-"
		if result.OperationID != "" {
			id = result.OperationID
			fingerprint = result.Fingerprint
		}
		pending, eligible := 0, 0
		if result.Pending {
			pending = 1
		}
		if result.Eligible {
			eligible = 1
		}
		_, err = fmt.Fprintf(output, "MSBOOST_BACKUP_ACTIVITY_INSPECT_V1\n%d\n%d\n%s\n%s\n%d\n", pending, eligible, id, fingerprint, result.StartedAt)
	} else {
		err = json.NewEncoder(output).Encode(result)
	}
	if err != nil {
		return errors.New("只读核对结果输出失败")
	}
	return nil
}

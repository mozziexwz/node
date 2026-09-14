package control

import (
	"errors"
	"time"
)

// BackupOperation is a durable admission record for work which outlives a
// database transaction. A crash does not prove that remote/file work ended:
// markers have no TTL and must not be cleared by another application's startup.
type BackupOperation struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	StartedAt    int64  `json:"startedAt"`
	LockIdentity string `json:"lockIdentity,omitempty"`
	LockDevice   string `json:"lockDevice,omitempty"`
	LockInode    string `json:"lockInode,omitempty"`
}

func backupOperationPending(s *State) bool {
	// Malformed records are still evidence of an unaccounted operation.
	return len(s.Docs["backup_operations"]) != 0
}

func beginBackupOperation(s *State, operation BackupOperation) error {
	if backupOperationPending(s) {
		return errors.New("存在尚未核实结束的站点备份操作；请先核对 backup_operations，不能按超时自动解除")
	}
	if _, exists := s.Docs["backups"][operation.ID]; exists {
		return errors.New("备份身份已有结果，拒绝重用")
	}
	if _, exists := s.Docs["backup_operation_reconciliations"][operation.ID]; exists {
		return errors.New("备份身份已有核销记录，拒绝重用")
	}
	return SaveDoc(s, "backup_operations", operation.ID, operation)
}

// Finish the activity and its result atomically. An error (including an unknown
// COMMIT result) is returned to the caller, never reinterpreted as success. The
// same random operation identity prevents one run from finishing another run.
func (b *BackupService) finishBackupOperation(operation BackupOperation, record *BackupRecord, runErr error, lock *backupActivityLock) error {
	return b.app.Store.Update(func(s *State) error {
		if lock.Validate() != nil || !backupActivityLockMatches(operation, lock) {
			return errBackupActivityReview
		}
		current, exists := LoadDoc[BackupOperation](s, "backup_operations", operation.ID)
		if !exists || current != operation {
			return errors.New("站点备份活动标记缺失或已改变，不能确认备份最终状态")
		}
		if record.ID == "" {
			if runErr == nil {
				return errors.New("站点备份缺少结果，不能报告完成")
			}
			*record = BackupRecord{ID: operation.ID, CreatedAt: time.Now().UnixMilli(), Status: "failed", Targets: map[string]string{}, Error: "本机备份副本写入失败；未开始异地上传，请检查存储及私有备份目录"}
		}
		if err := SaveDoc(s, "backups", record.ID, *record); err != nil {
			return err
		}
		DeleteDoc(s, "backup_operations", operation.ID)
		return nil
	})
}

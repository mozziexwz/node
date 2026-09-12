package control

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
)

func validBackupID(id string) bool {
	if id == "" || len(id) > 100 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (b *BackupService) deleteBackup(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var input struct {
		Confirm string `json:"confirm"`
	}
	if Decode(r, &input) != nil || input.Confirm != "DELETE_LOCAL" {
		Fail(w, 400, "请确认仅删除这份本机备份；不会删除远程副本或历史记录")
		return
	}
	if !b.mu.TryLock() {
		Fail(w, 409, "备份或恢复正在进行，当前禁止删除")
		return
	}
	defer b.mu.Unlock()
	if err := b.removeLocalBackup(r.PathValue("id")); err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "scope": "local", "message": "本机副本已删除，历史记录和远程副本保留。已删除的本机文件不能从本站撤销。"})
}

// The caller holds b.mu, excluding backup creation/restore/retention. Rename
// first so a database failure can restore the exact file; never remove it until
// the metadata transaction has committed.
func (b *BackupService) removeLocalBackup(id string) error {
	if !validBackupID(id) {
		return errors.New("备份标识无效")
	}
	var original, quarantine string
	err := b.app.Store.Update(func(s *State) error {
		record, ok := LoadDoc[BackupRecord](s, "backups", id)
		if !ok || record.ID != id {
			return errors.New("备份记录不存在")
		}
		if record.Protected {
			return errors.New("该副本为受保护的恢复前回滚备份，不能从网页删除")
		}
		if record.Targets["local"] == "removed" {
			return errors.New("本机副本已删除；远程副本未改变")
		}
		plan, _ := LoadDoc[BackupPlan](s, "backup_plans", "default")
		minimum := plan.MinCopies
		if minimum < 1 {
			minimum = 3
		}
		if b.verifiedLocalBackup(record) {
			remaining := 0
			for _, other := range ListDocs[BackupRecord](s, "backups") {
				if other.ID != id && b.verifiedLocalBackup(other) {
					remaining++
				}
			}
			if remaining < minimum {
				return errors.New("删除后有效本机副本少于保留策略的最少份数，请先创建更多有效备份")
			}
		}
		file := filepath.Join(b.app.Config.DataDir, "backups", id+".msb")
		if info, statErr := os.Lstat(file); statErr == nil {
			if !info.Mode().IsRegular() {
				return errors.New("备份路径不是普通文件，拒绝删除")
			}
			quarantine = filepath.Join(b.app.Config.DataDir, "backups", ".deleting-"+ID())
			if err := os.Rename(file, quarantine); err != nil {
				return errors.New("无法隔离待删除备份，请检查存储权限")
			}
			original = file
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
		if record.Targets == nil {
			record.Targets = map[string]string{}
		}
		record.Targets["local"], record.Status = "removed", "local_removed"
		contentAudit(s, "system", "backup.delete_local", id)
		return SaveDoc(s, "backups", id, record)
	})
	if err != nil {
		if original != "" {
			if renameErr := os.Rename(quarantine, original); renameErr != nil {
				return errors.New("删除失败且文件回迁失败；原文件仍在 backups/.deleting-* 私有隔离文件中，请人工检查")
			}
		}
		return err
	}
	if quarantine != "" {
		if err := os.Remove(quarantine); err != nil {
			return errors.New("下载副本已移除，但私有隔离文件无法删除，请检查 backups/.deleting-* 和存储权限")
		}
	}
	return nil
}

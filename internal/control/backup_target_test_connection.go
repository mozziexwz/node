package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func backupTargetIdentity(t BackupTarget) string {
	raw, _ := json.Marshal([]any{t.Host, t.Port, t.User, t.Path, t.Fingerprint, backupAuthMode(t.AuthMode), t.SealedKey, t.SealedPassword})
	return taskHash(raw)
}

func (b *BackupService) testTarget(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var target BackupTarget
	if err := b.app.Store.View(func(s *State) error {
		var ok bool
		target, ok = LoadDoc[BackupTarget](s, "backup_targets", r.PathValue("id"))
		if !ok {
			return errors.New("目标不存在")
		}
		return nil
	}); err != nil {
		Fail(w, 404, "目标不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// A unique, exclusively created tiny file exercises the same pinned SSH,
	// private directory, write and checksum readback path as an actual backup.
	// It is deleted after verification; no existing remote file is touched.
	probeErr := b.uploadTarget(ctx, target, ID(), []byte("MSBOOST backup connection test\n"), true)
	status := "passed"
	if probeErr != nil {
		status = "failed"
	}
	err := b.app.Store.Update(func(s *State) error {
		current, ok := LoadDoc[BackupTarget](s, "backup_targets", target.ID)
		if !ok || backupTargetIdentity(current) != backupTargetIdentity(target) {
			return errors.New("测试期间配置发生变化，请重新测试")
		}
		current.TestedAt, current.TestStatus = time.Now().UnixMilli(), status
		return SaveDoc(s, "backup_targets", current.ID, current)
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	if probeErr != nil {
		Fail(w, 502, "测试失败：请核对 SSH 密码或私钥、服务器指纹、网络及目录权限；没有将此目标标记为可用")
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "message": "SSH、写入和校验回读通过，测试文件已清理；实际备份是否成功请查看每次备份记录。"})
}

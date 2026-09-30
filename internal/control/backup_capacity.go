package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Every import/export path uses the SAME final encrypted-envelope budget.
// Reserve 20 MiB for execution results, accounting and ordinary state growth;
// attachments cannot consume the headroom needed to operate the site.
const backupMaxBytes = 100 << 20
const backupContentBudget = 80 << 20
const backupEnvelopeOverhead = 256
const backupMaxStateBytes = (backupMaxBytes-backupEnvelopeOverhead)*3/4 - 28

func backupPackedBudget(rawBytes int) int {
	// AES-GCM uses a 12-byte nonce + 16-byte tag; RawURL encoding has no padding.
	return base64.RawURLEncoding.EncodedLen(rawBytes+28) + backupEnvelopeOverhead
}

func checkContentBackupCapacity(s *State) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if size := backupPackedBudget(len(raw)); size > backupContentBudget {
		return fmt.Errorf("附件保存会超过可恢复备份的内容预算（80 MiB，另预留 20 MiB 给业务记录）。请删除不需要的附件后重试；本次上传未保存")
	}
	return nil
}

func (b *BackupService) capacity(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var size int
	err := b.app.Store.View(func(s *State) error {
		raw, err := json.Marshal(s)
		if err == nil {
			size = backupPackedBudget(len(raw))
		}
		return err
	})
	if err != nil {
		Fail(w, 503, "暂时无法读取备份容量")
		return
	}
	remaining := backupContentBudget - size
	if remaining < 0 {
		remaining = 0
	}
	status := "normal"
	if size >= backupContentBudget*9/10 {
		status = "warning"
	}
	if size > backupMaxBytes {
		status = "exceeded"
	}
	WriteJSON(w, 200, map[string]any{"estimatedPackedBytes": size, "maxPackedBytes": backupMaxBytes, "contentBudgetBytes": backupContentBudget, "remainingContentBytes": remaining, "status": status, "message": "容量按加密后的可恢复备份计算，不等于附件原始大小；接近上限时请清理无用附件和记录，不要只删除备份副本。"})
}

var errBackupStateCapacity = errors.New("此次保存会超过可恢复备份容量，未提交变更；请清理无用附件或联系管理员。删除以释放容量仍可进行")

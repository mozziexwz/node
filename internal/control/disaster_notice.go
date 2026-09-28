package control

import (
	"context"
	"fmt"
	"github.com/mozziexwz/node/internal/disaster"
	"html"
	"time"
)

const disasterHistoryFile = "/app/disaster-status/backup-history.json"

type disasterNotice struct {
	Event    string          `json:"event"`
	RetryAt  int64           `json:"retryAt"`
	Attempts int             `json:"attempts"`
	Sent     map[string]bool `json:"sent"`
}

func (b *BackupService) notifyDisasterFailure(ctx context.Context) {
	h, err := disaster.ReadHistory(disasterHistoryFile)
	if err != nil || len(h.Runs) == 0 {
		return
	}
	b.notifyDisasterRun(ctx, h.Runs[0])
}

func (b *BackupService) notifyDisasterRun(ctx context.Context, r disaster.BackupRun) {
	if r.Stage != "failed" && r.Stage != "remote_failed" && r.Stage != "cleanup_warning" {
		return
	}
	event := fmt.Sprintf("%s:%d", r.ID, r.UpdatedAt)
	var recipients []string
	var config SMTPConfig
	now := time.Now().UnixMilli()
	err := b.app.Store.Update(func(s *State) error {
		if !boolSetting(s, "disasterBackupEmailEnabled") || !smtpReady(s) {
			return nil
		}
		n, _ := LoadDoc[disasterNotice](s, "disaster_backup_notice", "default")
		if n.Event != event {
			n = disasterNotice{Event: event, Sent: map[string]bool{}}
		}
		if n.RetryAt > now || n.Attempts >= 3 {
			return nil
		}
		for _, u := range s.Users {
			if u.Role == "admin" && u.Status == "active" && !n.Sent[u.Email] {
				recipients = append(recipients, u.Email)
			}
		}
		if len(recipients) == 0 {
			return nil
		}
		config, _ = getSMTP(s)
		n.Attempts++
		n.RetryAt = now + int64(30*time.Minute/time.Millisecond)
		return SaveDoc(s, "disaster_backup_notice", "default", n)
	})
	if err != nil || len(recipients) == 0 {
		return
	}
	secret, err := b.app.Open(config.Secret)
	if err != nil {
		return
	}
	defer clear(secret)
	// Only fixed result labels and a safe timestamp, never filenames, hostnames,
	// secret material, remote stderr or logs are included in email.
	label := "本地备份未完成"
	if r.LocalOK {
		label = "本地备份已校验，异地上传未成功"
	}
	if r.Stage == "cleanup_warning" {
		label = "新备份已保存，旧备份清理未完成"
	}
	body := "整站备份需要处理：" + label + "。\n时间：" + emailTimeLabel(r.UpdatedAt) + "\n请登录后台“备份与恢复 → 整站备份状态”，或在面板服务器运行 msboost 查看备份记录。无需停止网站。"
	msg := EmailMessage{Subject: "MSBOOST 整站备份提醒", TextBody: body, HTMLBody: "<p>" + html.EscapeString(body) + "</p>"}
	for _, recipient := range recipients {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		e := b.app.mailSender(sendCtx, config, string(secret), recipient, msg)
		cancel()
		if e != nil {
			continue
		}
		_ = b.app.Store.Update(func(s *State) error {
			n, _ := LoadDoc[disasterNotice](s, "disaster_backup_notice", "default")
			if n.Event != event {
				return nil
			}
			if n.Sent == nil {
				n.Sent = map[string]bool{}
			}
			n.Sent[recipient] = true
			return SaveDoc(s, "disaster_backup_notice", "default", n)
		})
	}
}

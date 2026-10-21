package control

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The email log keeps one record per message the site tried to send, so an
// administrator can tell whether a mail went out. A record holds the kind,
// the full recipient, the fixed template subject, the result and a bounded
// failure category: never a body, code, link, SMTP credential or raw SMTP
// reply. "sent" means the SMTP server accepted the message, not that it
// reached an inbox.
const (
	emailLogCollection = "email_log"
	emailLogMaxAge     = 180 * 24 * time.Hour
	backupAlertKind    = "backup_alert"
)

type emailKind struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// emailKinds names every kind of mail the site sends, in display order.
var emailKinds = []emailKind{
	{"register", "注册验证码"},
	{"verify", "邮箱验证"},
	{"password_reset", "找回密码"},
	{"password_changed", "密码已修改"},
	{"relay_code_used", "中转码使用提醒"},
	{"relay_verify_failed", "中转线路校验未通过"},
	{"relay_verify_refused", "新配置的中转线路已撤销"},
	{"relay_verify_paused", "中转线路已暂停"},
	{"smtp_test", "SMTP 测试"},
	{backupAlertKind, "整站备份提醒"},
}

func emailKindLabel(kind string) string {
	for _, k := range emailKinds {
		if k.Kind == kind {
			return k.Label
		}
	}
	return ""
}

type emailLogRecord struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Recipient string `json:"recipient"`
	UserID    string `json:"userId,omitempty"`
	Subject   string `json:"subject"`
	Status    string `json:"status"`
	Failure   string `json:"failure,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// logEmail records one delivery attempt after it has finished. Saving the
// record never changes the attempt's outcome: a record that cannot be saved,
// for example during a whole-site backup, only goes to the server log.
func (a *App) logEmail(kind, userID, recipient, subject string, err error) {
	now := time.Now()
	record := emailLogRecord{ID: ID(), Kind: kind, Label: emailKindLabel(kind), Recipient: recipient, UserID: userID, Subject: subject, Status: "sent", CreatedAt: now.UnixMilli()}
	if err != nil {
		record.Status, record.Failure = "failed", mailFailureCategory(err)
	}
	saveErr := a.Store.Update(func(s *State) error {
		pruneEmailLog(s, now)
		return SaveDoc(s, emailLogCollection, record.ID, record)
	})
	if saveErr != nil {
		log.Printf("email log: %s record not saved: %v", kind, saveErr)
	}
}

// pruneEmailLog drops records past the retention window, then the oldest
// beyond the audit count bound. Mail is rare next to other writes, so reading
// every record's age on each write stays cheap.
func pruneEmailLog(s *State, now time.Time) {
	cutoff := now.Add(-emailLogMaxAge).UnixMilli()
	for id, raw := range s.Docs[emailLogCollection] {
		if auditCreatedAt(raw) < cutoff {
			DeleteDoc(s, emailLogCollection, id)
		}
	}
	pruneAudit(s, emailLogCollection)
}

// StartEmailLog also expires records while no mail is being sent.
func (a *App) StartEmailLog(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_ = a.Store.Update(func(s *State) error { pruneEmailLog(s, now); return nil })
			}
		}
	}()
}

// emailLog lists records newest first. total counts every record matching the
// filters, including those beyond limit.
func (a *App) emailLog(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		Fail(w, 403, err.Error())
		return
	}
	query := r.URL.Query()
	kind, status := query.Get("kind"), query.Get("status")
	search := strings.ToLower(strings.TrimSpace(query.Get("q")))
	if kind != "" && emailKindLabel(kind) == "" {
		Fail(w, 400, "邮件类型无效")
		return
	}
	if status != "" && status != "sent" && status != "failed" {
		Fail(w, 400, "发送结果只能是 sent 或 failed")
		return
	}
	if utf8.RuneCountInString(search) > 254 {
		Fail(w, 400, "搜索内容过长")
		return
	}
	limit := 200
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			Fail(w, 400, "limit 需为 1–1000")
			return
		}
		limit = n
	}
	// Records past the window stay hidden until the next prune removes them.
	cutoff := time.Now().Add(-emailLogMaxAge).UnixMilli()
	records := []emailLogRecord{}
	err := a.Store.View(func(s *State) error {
		for _, record := range ListDocs[emailLogRecord](s, emailLogCollection) {
			if record.CreatedAt < cutoff || kind != "" && record.Kind != kind || status != "" && record.Status != status ||
				search != "" && !strings.Contains(strings.ToLower(record.Recipient), search) && !strings.Contains(strings.ToLower(record.Subject), search) {
				continue
			}
			// The current name keeps rows and the kind filter in step.
			if label := emailKindLabel(record.Kind); label != "" {
				record.Label = label
			}
			records = append(records, record)
		}
		return nil
	})
	if err != nil {
		Fail(w, 500, "邮件记录读取失败")
		return
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt != records[j].CreatedAt {
			return records[i].CreatedAt > records[j].CreatedAt
		}
		return records[i].ID > records[j].ID
	})
	WriteJSON(w, 200, map[string]any{
		"records": records[:min(limit, len(records))], "total": len(records), "kinds": emailKinds,
		"retentionDays": int(emailLogMaxAge / (24 * time.Hour)), "maxRecords": auditRetain,
	})
}

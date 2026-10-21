package control

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/disaster"
)

// emailOutbox stands in for the SMTP server and keeps what it accepted.
type emailOutbox struct {
	mu   sync.Mutex
	sent []emailOutboxItem
}

type emailOutboxItem struct {
	to      string
	message EmailMessage
}

func (b *emailOutbox) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sent)
}

// code is the six-digit code of the latest mail with this subject.
func (b *emailOutbox) code(t *testing.T, subject string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.sent) - 1; i >= 0; i-- {
		if b.sent[i].message.Subject == subject {
			if code := regexp.MustCompile(`[0-9]{6}`).FindString(b.sent[i].message.TextBody); code != "" {
				return code
			}
		}
	}
	t.Fatalf("no code mailed under %s", subject)
	return ""
}

func emailLogFixture(t *testing.T) (*App, http.Handler, *emailOutbox) {
	t.Helper()
	a, h := identityFixture(t, true)
	sealed, err := a.Seal([]byte("mail-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Store.Update(func(s *State) error {
		s.Settings["smtp"] = true
		s.Settings["smtpConfig"] = SMTPConfig{Host: "smtp.example.com", Port: 465, Sender: "12345678@qq.com", Name: "MSBOOST", Encryption: "tls", Secret: sealed, TestedAt: time.Now().UnixMilli()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	box := &emailOutbox{}
	a.mailSender = func(_ context.Context, _ SMTPConfig, secret, recipient string, message EmailMessage) error {
		if secret != "mail-secret" {
			return errors.New("incorrect decrypted SMTP secret")
		}
		box.mu.Lock()
		defer box.mu.Unlock()
		box.sent = append(box.sent, emailOutboxItem{recipient, message})
		return nil
	}
	return a, h, box
}

func emailLogRecords(t *testing.T, a *App) []emailLogRecord {
	t.Helper()
	var records []emailLogRecord
	if err := a.Store.View(func(s *State) error {
		records = ListDocs[emailLogRecord](s, emailLogCollection)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return records
}

func emailLogConfig(t *testing.T, a *App) SMTPConfig {
	t.Helper()
	var config SMTPConfig
	if err := a.Store.View(func(s *State) error {
		var err error
		config, err = getSMTP(s)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestEmailLogRecordsEverySendPath(t *testing.T) {
	a, h, box := emailLogFixture(t)
	admin, adminCSRF := identityLoginAdmin(t, h)
	member, memberCSRF, user := identityRegister(t, h, "12345678@qq.com")
	memberID, adminID := user["id"].(string), ""
	if err := a.Store.Update(func(s *State) error {
		s.Settings["registrationEmailVerificationRequired"] = true
		s.Settings["relayCodes"] = map[string]any{"notifyOwner": true}
		s.Settings["disasterBackupEmailEnabled"] = true
		for _, u := range s.Users {
			if u.Role == "admin" {
				adminID = u.ID
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "register", "email": "20000001@qq.com"}, nil, ""), 200)
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "verify"}, member, memberCSRF), 200)
	identityResponse(t, resetRequest(h, "12345678@qq.com"), 200)
	a.passwordRecovery.wg.Wait()
	identityResponse(t, resetConfirm(h, "12345678@qq.com", box.code(t, "[MSBOOST] 找回密码验证码"), resetTestPassword), 200)
	a.passwordRecovery.wg.Wait()
	// Route verification mail goes only to verified addresses.
	if err := a.Store.Update(func(s *State) error { s.Users[memberID].EmailVerifiedAt = time.Now().UnixMilli(); return nil }); err != nil {
		t.Fatal(err)
	}
	NewTaskService(a).notifyRelayCodeOwner(relayCodeNotice{OwnerID: memberID, At: time.Now().UnixMilli()})
	for _, kind := range []string{"relay_verify_failed", "relay_verify_refused", "relay_verify_paused"} {
		a.notifyRuleVerify(verifyNotice{userID: memberID, kind: kind, at: time.Now().UnixMilli()})
	}
	a.passwordRecovery.wg.Wait()
	identityResponse(t, identityRequest(h, "POST", "/api/admin/smtp/test", map[string]any{}, admin, adminCSRF), 200)
	NewBackupService(a).notifyDisasterRun(context.Background(), disaster.BackupRun{ID: "run", Stage: "failed", UpdatedAt: time.Now().UnixMilli()})

	want := map[string]struct{ userID, recipient string }{
		"register":             {"", "20000001@qq.com"},
		"verify":               {memberID, "12345678@qq.com"},
		"password_reset":       {memberID, "12345678@qq.com"},
		"password_changed":     {memberID, "12345678@qq.com"},
		"relay_code_used":      {memberID, "12345678@qq.com"},
		"relay_verify_failed":  {memberID, "12345678@qq.com"},
		"relay_verify_refused": {memberID, "12345678@qq.com"},
		"relay_verify_paused":  {memberID, "12345678@qq.com"},
		"smtp_test":            {"", "99999999@qq.com"},
		backupAlertKind:        {adminID, "99999999@qq.com"},
	}
	// A new kind of mail needs its sending path exercised here as well.
	for _, k := range emailKinds {
		if _, ok := want[k.Kind]; !ok {
			t.Fatalf("no send path covers %s", k.Kind)
		}
	}
	records := emailLogRecords(t, a)
	if len(records) != len(want) || box.count() != len(want) {
		t.Fatalf("%d records for %d mails, want %d", len(records), box.count(), len(want))
	}
	for _, r := range records {
		w, ok := want[r.Kind]
		if !ok || r.UserID != w.userID || r.Recipient != w.recipient || r.Status != "sent" || r.Failure != "" ||
			r.Label == "" || r.Label != emailKindLabel(r.Kind) || r.ID == "" || r.CreatedAt == 0 {
			t.Fatalf("unexpected record %+v", r)
		}
		delete(want, r.Kind)
		found := false
		for _, mail := range box.sent {
			found = found || mail.to == r.Recipient && mail.message.Subject == r.Subject
		}
		if !found {
			t.Fatalf("record %+v does not match a sent mail", r)
		}
	}
	if len(want) != 0 {
		t.Fatalf("no record for %v", want)
	}
}

type emailTimeoutError struct{}

func (emailTimeoutError) Error() string   { return "i/o timeout" }
func (emailTimeoutError) Timeout() bool   { return true }
func (emailTimeoutError) Temporary() bool { return true }

func TestEmailLogFailureCategories(t *testing.T) {
	a, h, _ := emailLogFixture(t)
	config := emailLogConfig(t, a)
	broken := config
	broken.Secret = "not-a-sealed-secret"
	event := EmailData{EventAtLabel: emailTimeLabel(time.Now().UnixMilli())}
	for _, test := range []struct {
		name   string
		config SMTPConfig
		data   EmailData
		smtp   error
		want   string
	}{
		{"rendering", config, EmailData{}, nil, "render_failure"},
		{"credential", broken, event, nil, "credential_decrypt_failure"},
		{"authentication", config, event, errors.New("535 5.7.8 authentication failed reply-detail"), "authentication_failure"},
		{"timeout", config, event, emailTimeoutError{}, "network_timeout"},
		{"network", config, event, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused reply-detail")}, "network_failure"},
		{"certificate", config, event, &tls.CertificateVerificationError{Err: errors.New("unknown authority reply-detail")}, "tls_certificate_failure"},
		{"rejected", config, event, errors.New("550 5.1.1 mailbox unavailable reply-detail"), "delivery_failure"},
		{"accepted", config, event, nil, ""},
	} {
		if err := a.Store.Update(func(s *State) error { delete(s.Docs, emailLogCollection); return nil }); err != nil {
			t.Fatal(err)
		}
		calls := 0
		a.mailSender = func(context.Context, SMTPConfig, string, string, EmailMessage) error {
			calls++
			return test.smtp
		}
		kind := "relay_verify_paused"
		if test.name == "rendering" {
			kind = "register" // a code mail without a code
		}
		err := a.sendAccountEmail(context.Background(), test.config, "user-1", "12345678@qq.com", kind, test.data)
		if (err == nil) != (test.want == "") || err != nil && mailFailureCategory(err) != test.want {
			t.Fatalf("%s: %v", test.name, err)
		}
		if (test.name == "rendering" || test.name == "credential") != (calls == 0) {
			t.Fatalf("%s: %d SMTP calls", test.name, calls)
		}
		records := emailLogRecords(t, a)
		status := "sent"
		if test.want != "" {
			status = "failed"
		}
		if len(records) != 1 || records[0].Status != status || records[0].Failure != test.want || records[0].Kind != kind || records[0].UserID != "user-1" {
			t.Fatalf("%s: %+v", test.name, records)
		}
		raw, _ := json.Marshal(records)
		if strings.Contains(string(raw), "reply-detail") || strings.Contains(string(raw), "cipher") {
			t.Fatalf("%s: raw error kept: %s", test.name, raw)
		}
	}

	// The administrator audit reads the same category as the log.
	if err := a.Store.Update(func(s *State) error {
		s.Settings["registrationEmailVerificationRequired"] = true
		s.Settings["smtpConfig"] = broken
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "register", "email": "20000001@qq.com"}, nil, ""), 502)
	if err := a.Store.View(func(s *State) error {
		for _, event := range ListDocs[map[string]any](s, "audit") {
			if event["action"] == "smtp.delivery.failed" && event["reason"] == "credential_decrypt_failure" {
				return nil
			}
		}
		return errors.New("an undecryptable credential was audited as another failure")
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEmailLogKeepsNoCodesLinksOrSecrets(t *testing.T) {
	a, h, box := emailLogFixture(t)
	member, csrf, _ := identityRegister(t, h, "12345678@qq.com")
	if err := a.Store.Update(func(s *State) error { s.Settings["registrationEmailVerificationRequired"] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "register", "email": "20000001@qq.com"}, nil, ""), 200)
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "verify"}, member, csrf), 200)
	identityResponse(t, resetRequest(h, "12345678@qq.com"), 200)
	a.passwordRecovery.wg.Wait()
	codes := []string{box.code(t, "[MSBOOST] 注册验证码"), box.code(t, "[MSBOOST] 邮箱验证"), box.code(t, "[MSBOOST] 找回密码验证码")}
	sealed := emailLogConfig(t, a).Secret
	var raw []byte
	var stored map[string]map[string]any
	if err := a.Store.View(func(s *State) error {
		raw, _ = json.Marshal(s.Docs[emailLogCollection])
		return json.Unmarshal(raw, &stored)
	}); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Fatalf("%d records", len(stored))
	}
	for _, secret := range []string{"mail-secret", sealed, "https://", "identity.test", "href", "12***@qq.com"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("email log keeps %q", secret)
		}
	}
	allowed := map[string]bool{"id": true, "kind": true, "label": true, "recipient": true, "userId": true, "subject": true, "status": true, "failure": true, "createdAt": true}
	for _, record := range stored {
		for key, value := range record {
			if !allowed[key] {
				t.Fatalf("unexpected field %s", key)
			}
			text, _ := value.(string)
			for _, code := range codes {
				// The recipient and random ID are fixed elsewhere; any field that
				// could carry message content must not hold a code.
				if key != "id" && key != "recipient" && strings.Contains(text, code) {
					t.Fatalf("%s keeps a mailed code", key)
				}
			}
		}
	}
}

func TestEmailLogPrunesByAgeAndCount(t *testing.T) {
	now := time.Now()
	s := newState()
	put := func(id string, at time.Time) {
		if err := SaveDoc(s, emailLogCollection, id, emailLogRecord{ID: id, Kind: "register", Status: "sent", CreatedAt: at.UnixMilli()}); err != nil {
			t.Fatal(err)
		}
	}
	put("expired", now.Add(-emailLogMaxAge-time.Minute))
	put("inside", now.Add(-emailLogMaxAge+time.Minute))
	pruneEmailLog(s, now)
	if _, ok := s.Docs[emailLogCollection]["expired"]; ok {
		t.Fatal("record older than 180 days kept")
	}
	if _, ok := s.Docs[emailLogCollection]["inside"]; !ok {
		t.Fatal("record inside the window removed")
	}
	bulk := auditRetain + auditPruneSlack
	for i := 0; i < bulk; i++ {
		put(fmt.Sprintf("bulk-%05d", i), now.Add(time.Duration(i-bulk)*time.Second))
	}
	pruneEmailLog(s, now)
	docs := s.Docs[emailLogCollection]
	if len(docs) != auditRetain {
		t.Fatalf("%d records kept", len(docs))
	}
	if _, ok := docs["inside"]; ok {
		t.Fatal("oldest record survived the count bound")
	}
	if _, ok := docs[fmt.Sprintf("bulk-%05d", bulk-1)]; !ok {
		t.Fatal("newest record removed")
	}

	// Each new record prunes, and the list never shows an expired one.
	a, h, _ := emailLogFixture(t)
	admin, _ := identityLoginAdmin(t, h)
	if err := a.Store.Update(func(s *State) error {
		return SaveDoc(s, emailLogCollection, "old", emailLogRecord{ID: "old", Kind: "register", Recipient: "20000001@qq.com", Status: "sent", CreatedAt: now.Add(-emailLogMaxAge - time.Hour).UnixMilli()})
	}); err != nil {
		t.Fatal(err)
	}
	if out := identityResponse(t, identityRequest(h, "GET", "/api/admin/email-log", nil, admin, ""), 200); out["total"] != 0.0 {
		t.Fatalf("expired record listed: %v", out)
	}
	a.logEmail("smtp_test", "", "99999999@qq.com", "[MSBOOST] 邮件服务测试", nil)
	if records := emailLogRecords(t, a); len(records) != 1 || records[0].ID == "old" {
		t.Fatalf("expired record kept after a new one: %+v", records)
	}
}

func TestEmailLogAdminOnlyWithFilters(t *testing.T) {
	a, h, _ := emailLogFixture(t)
	admin, _ := identityLoginAdmin(t, h)
	member, _, _ := identityRegister(t, h, "12345678@qq.com")
	now := time.Now().UnixMilli()
	if err := a.Store.Update(func(s *State) error {
		for _, r := range []emailLogRecord{
			{ID: "a", Kind: "register", Label: "注册验证码", Recipient: "20000001@qq.com", Subject: "[MSBOOST] 注册验证码", Status: "sent", CreatedAt: now - 4000},
			{ID: "b", Kind: "password_reset", Label: "找回密码", Recipient: "12345678@qq.com", UserID: "u1", Subject: "[MSBOOST] 找回密码验证码", Status: "failed", Failure: "authentication_failure", CreatedAt: now - 3000},
			{ID: "c", Kind: "smtp_test", Label: "SMTP 测试", Recipient: "Admin@Example.com", Subject: "[MSBOOST] 邮件服务测试", Status: "sent", CreatedAt: now - 2000},
			{ID: "d", Kind: "register", Label: "旧名称", Recipient: "20000002@qq.com", Subject: "[MSBOOST] 注册验证码", Status: "failed", Failure: "network_timeout", CreatedAt: now - 1000},
		} {
			if err := SaveDoc(s, emailLogCollection, r.ID, r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	get := func(cookie *http.Cookie, query string, status int) map[string]any {
		t.Helper()
		return identityResponse(t, identityRequest(h, "GET", "/api/admin/email-log"+query, nil, cookie, ""), status)
	}
	ids := func(out map[string]any) string {
		var list []string
		for _, r := range out["records"].([]any) {
			list = append(list, r.(map[string]any)["id"].(string))
		}
		return strings.Join(list, ",")
	}
	// The role check comes before any parameter is read.
	for _, query := range []string{"", "?kind=bogus"} {
		get(nil, query, 403)
		get(member, query, 403)
	}
	out := get(admin, "", 200)
	if ids(out) != "d,c,b,a" || out["total"] != 4.0 || out["retentionDays"] != 180.0 || out["maxRecords"] != float64(auditRetain) || len(out["kinds"].([]any)) != len(emailKinds) {
		t.Fatalf("unexpected list: %v", out)
	}
	if first := out["records"].([]any)[0].(map[string]any); first["label"] != "注册验证码" || first["failure"] != "network_timeout" {
		t.Fatalf("label not refreshed or failure missing: %v", first)
	}
	for query, want := range map[string]string{
		"?kind=register":                              "d,a",
		"?status=failed":                              "d,b",
		"?kind=register&status=failed":                "d",
		"?q=ADMIN%40example":                          "c",
		"?q=" + url.QueryEscape("找回密码"):               "b",
		"?q=" + url.QueryEscape(" 2000000 "):          "d,a",
		"?status=sent&q=" + url.QueryEscape("qq.com"): "a",
	} {
		if got := ids(get(admin, query, 200)); got != want {
			t.Fatalf("%s: got %q, want %q", query, got, want)
		}
	}
	if limited := get(admin, "?limit=2", 200); ids(limited) != "d,c" || limited["total"] != 4.0 {
		t.Fatalf("limit: %v", limited)
	}
	for _, query := range []string{"?kind=bogus", "?status=queued", "?limit=0", "?limit=1001", "?limit=x", "?q=" + strings.Repeat("a", 255)} {
		get(admin, query, 400)
	}
}

func TestEmailLogFailureNeverBlocksDelivery(t *testing.T) {
	a, _, box := emailLogFixture(t)
	config := emailLogConfig(t, a)
	gate := func(on bool) {
		t.Helper()
		if err := a.Store.transactionWithBackupPause(true, true, func(s *State) error {
			if on {
				s.Docs[backupPauseCollection] = map[string]json.RawMessage{"default": json.RawMessage(`{}`)}
			} else {
				delete(s.Docs, backupPauseCollection)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A whole-site backup refuses every write, including the record.
	gate(true)
	if err := a.sendAccountEmail(context.Background(), config, "", "99999999@qq.com", "smtp_test", EmailData{EventAtLabel: emailTimeLabel(time.Now().UnixMilli())}); err != nil {
		t.Fatalf("delivery failed because its record could not be saved: %v", err)
	}
	gate(false)
	if box.count() != 1 || len(emailLogRecords(t, a)) != 0 {
		t.Fatalf("%d mails, %d records", box.count(), len(emailLogRecords(t, a)))
	}
}

func TestEmailKindsNameEveryTemplate(t *testing.T) {
	raw, err := emailTemplates.ReadFile("email_templates/messages.zh-CN.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]json.RawMessage
	if err = json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	for kind := range catalog {
		if emailKindLabel(kind) == "" {
			t.Errorf("template %s has no name in the email log", kind)
		}
	}
	// Every template plus the whole-site backup alert, which has none.
	if len(emailKinds) != len(catalog)+1 || emailKindLabel(backupAlertKind) == "" {
		t.Fatalf("%d names for %d templates", len(emailKinds), len(catalog))
	}
	for _, k := range emailKinds {
		if strings.Contains(k.Label, "转发") || strings.Contains(k.Label, "冒险岛") {
			t.Fatalf("label %s", k.Label)
		}
	}
}

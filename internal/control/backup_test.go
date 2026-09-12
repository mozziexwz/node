package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupRetentionDoesNotCountCorruptCopyAsRecoverable(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	raw := backupSnapshot(t, a, b)
	records := []BackupRecord{}
	for i := 0; i < 3; i++ {
		record, err := b.write(raw)
		if err != nil {
			t.Fatal(err)
		}
		record.CreatedAt = time.Now().Add(-90*24*time.Hour).UnixMilli() - int64(i)
		records = append(records, record)
	}
	if err := os.WriteFile(filepath.Join(a.Config.DataDir, "backups", records[0].ID+".msb"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	err := a.Store.Update(func(s *State) error {
		for _, record := range records {
			if err := SaveDoc(s, "backups", record.ID, record); err != nil {
				return err
			}
		}
		return SaveDoc(s, "backup_plans", "default", BackupPlan{MinCopies: 2, RetentionDays: 30})
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	b.Register(mux)
	w := commerceTestRequest(mux, &User{ID: "admin-context", Role: "admin", Status: "active"}, "POST", "/api/admin/backups/retention", map[string]any{"confirm": true})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, record := range records[1:] {
		if !b.verifiedLocalBackup(record) {
			t.Fatal("retention deleted a required recoverable copy")
		}
	}
}

func backupRestoreRequest(t *testing.T, b *BackupService, admin *User, raw []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	f, err := writer.CreateFormFile("file", "backup.msb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(raw); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	writer.WriteField("confirm", "RESTORE")
	writer.WriteField("sha256", hex.EncodeToString(sum[:]))
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/admin/backups/restore", &body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, &requestIdentity{user: admin}))
	w := httptest.NewRecorder()
	b.restore(w, r)
	return w
}
func backupSnapshot(t *testing.T, a *App, b *BackupService) []byte {
	t.Helper()
	var raw []byte
	err := a.Store.View(func(s *State) error { var err error; raw, err = b.pack(s); return err })
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestRestoreRejectsRedeemedCardFinancialRewind(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	b := NewBackupService(a)
	admin := &User{ID: "admin-context", Role: "admin", Status: "active"}
	code := "MSB-BACKUP-CARD"
	sealed, err := a.Seal([]byte(code))
	if err != nil {
		t.Fatal(err)
	}
	err = a.Store.Update(func(s *State) error {
		return SaveDoc(s, "cards", "card", BalanceCard{ID: "card", Status: "active", AmountCents: 800, CodeHash: commerceHash(code), SealedCode: sealed})
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := backupSnapshot(t, a, b)
	w := commerceTestRequest(mux, u, "POST", "/api/wallet/redeem", map[string]any{"code": code, "requestId": "first-redemption"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err = a.Store.Update(func(s *State) error { s.Settings["maintenance"] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	w = backupRestoreRequest(t, b, admin, raw)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "财务") {
		t.Fatalf("financial rewind: %d %s", w.Code, w.Body.String())
	}
	err = a.Store.Update(func(s *State) error {
		c, _ := LoadDoc[BalanceCard](s, "cards", "card")
		if c.Status != "used" || c.UsedBy != u.ID || s.Users[u.ID].BalanceCents != 800 {
			t.Error("rejected restore still mutated money/card")
		}
		s.Settings["maintenance"] = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w = commerceTestRequest(mux, u, "POST", "/api/wallet/redeem", map[string]any{"code": code, "requestId": "second-redemption"})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "已被使用") {
		t.Fatal(w.Body.String())
	}
	err = a.Store.View(func(s *State) error {
		if s.Users[u.ID].BalanceCents != 800 {
			t.Error("card was redeemed a second time")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestRestoreRetainsUnexpiredEncryptedDownloadAndDropsExpired(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	b := NewBackupService(a)
	admin := &User{ID: "admin-context", Role: "admin", Status: "active"}
	now := time.Now().UnixMilli()
	sealed, err := a.Seal([]byte(relayTestConfig))
	if err != nil {
		t.Fatal(err)
	}
	err = a.Store.Update(func(s *State) error {
		s.Users[u.ID].ExpiresAt = now + commerceDay
		s.Users[u.ID].TrafficTotal = commerceGB
		s.Users["expired"] = &User{ID: "expired", Role: "member", Status: "active", ExpiresAt: now - 1, TrafficTotal: commerceGB}
		for _, id := range []string{u.ID, "expired"} {
			rule := UserRule{ID: "rule-" + id, UserID: id, RouteID: "route", State: "paused", SealedConfig: sealed, Version: 2, Segments: []RelaySegment{{AgentID: "old-agent", LastLease: now - 1000}}}
			if err := SaveDoc(s, "user_rules", id+":route", rule); err != nil {
				return err
			}
		}
		return SaveDoc(s, "routes", "route", Route{ID: "route", Enabled: true, EntryAgentID: "old-agent"})
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := backupSnapshot(t, a, b)
	if err = a.Store.Update(func(s *State) error { s.Settings["maintenance"] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	w := backupRestoreRequest(t, b, admin, raw)
	if w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	err = a.Store.View(func(s *State) error {
		valid, ok := LoadDoc[UserRule](s, "user_rules", u.ID+":route")
		if !ok || valid.SealedConfig == "" || valid.State != "paused" || len(valid.Segments) != 0 {
			t.Error("valid config was deleted or runtime not isolated")
		}
		if _, ok := LoadDoc[UserRule](s, "user_rules", "expired:route"); ok {
			t.Error("expired config survived restore")
		}
		route, _ := LoadDoc[Route](s, "routes", "route")
		if route.Enabled {
			t.Error("restored route must await Agent reassociation")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w = commerceTestRequest(mux, u, "GET", "/api/user/routes/route/config", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "secret-password") {
		t.Fatalf("restored valid file not downloadable: %d %s", w.Code, w.Body.String())
	}
	w = commerceTestRequest(mux, &User{ID: "expired", Status: "active"}, "GET", "/api/user/routes/route/config", nil)
	if w.Code == 200 {
		t.Fatal("expired restored download disclosed")
	}
}

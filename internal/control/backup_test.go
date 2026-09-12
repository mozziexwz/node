package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	if err := b.app.Store.View(func(s *State) error {
		fingerprint, err := restoreScopeFingerprint(s)
		if err == nil {
			err = writer.WriteField("currentSha256", fingerprint)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
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
func TestSafeRestorePreservesRedeemedCardAndCurrentFinancialState(t *testing.T) {
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
	if w.Code != 200 {
		t.Fatalf("safe restore failed after financial change: %d %s", w.Code, w.Body.String())
	}
	err = a.Store.Update(func(s *State) error {
		c, _ := LoadDoc[BalanceCard](s, "cards", "card")
		if c.Status != "used" || c.UsedBy != u.ID || s.Users[u.ID].BalanceCents != 800 {
			t.Error("safe restore rewound money/card")
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
func TestRestoreRetainsRuleIdentityAndRejectsExpiredDownload(t *testing.T) {
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
		if err := SaveDoc(s, "relay_agents", "old-agent", RelayAgent{ID: "old-agent", TokenHash: "old-token", Enabled: true}); err != nil {
			return err
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
		if expired, ok := LoadDoc[UserRule](s, "user_rules", "expired:route"); !ok || expired.State != "paused" {
			t.Error("restore must preserve metered rule identity even when download is expired")
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

func TestSafeRestoreAllowlistPreservesIdentityMoneyTrafficAndRouteTopology(t *testing.T) {
	a, _, u := commerceTestApp(t)
	b := NewBackupService(a)
	err := a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "articles", "guide", Article{ID: "guide", Title: "old guide"}); err != nil {
			return err
		}
		if err := SaveDoc(s, "relay_agents", "node", RelayAgent{ID: "node", Name: "snapshot node", TokenHash: "never-restore-token", Enabled: true}); err != nil {
			return err
		}
		if err := SaveDoc(s, "routes", "route", Route{ID: "route", EntryAgentID: "node", Name: "snapshot route"}); err != nil {
			return err
		}
		return SaveDoc(s, "routes", "unassociated", Route{ID: "unassociated", EntryAgentID: "node", Name: "restore deleted route"})
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := backupSnapshot(t, a, b)
	err = a.Store.Update(func(s *State) error {
		s.Settings["maintenance"] = true
		s.Users[u.ID].Email = "updated@example.com"
		s.Users[u.ID].BalanceCents, s.Users[u.ID].TrafficUsed, s.Users[u.ID].ExpiresAt = 800, 111, time.Now().Add(time.Hour).UnixMilli()
		s.Users["new-user"] = &User{ID: "new-user", Email: "new@example.com", Status: "active"}
		for _, name := range []string{"orders", "ledger", "cards", "order_requests", "redeem_requests", "payment_trades", "payment_callbacks", "entitlement_versions", "traffic_cursors", "traffic_months", "relay_sync_sequences", "relay_rule_archive", "user_targets", "tickets", "invitations", "future_financial_collection"} {
			if err := SaveDoc(s, name, "preserve", map[string]any{"value": 123, "userId": u.ID}); err != nil {
				return err
			}
		}
		DeleteDoc(s, "articles", "guide")
		DeleteDoc(s, "routes", "unassociated")
		if err := SaveDoc(s, "routes", "route", Route{ID: "route", EntryAgentID: "node", Name: "current topology"}); err != nil {
			return err
		}
		if err := SaveDoc(s, "relay_agents", "node", RelayAgent{ID: "node", Name: "current node", Enabled: true, TokenHash: "current-token"}); err != nil {
			return err
		}
		return SaveDoc(s, "user_rules", u.ID+":route", UserRule{ID: "new-rule", UserID: u.ID, RouteID: "route", TrafficBytes: 123, State: "paused"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var before *State
	_ = a.Store.View(func(s *State) error { before, _ = cloneState(s); return nil })
	snapshot, _ := b.unpack(raw)
	_, preflight, err := mergeSafeRestore(before, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(preflight.PreservedRoutes) != 1 || preflight.PreservedRoutes[0] != "route" {
		t.Fatalf("missing reference warning: %+v", preflight)
	}
	w := backupRestoreRequest(t, b, &User{Role: "admin", Status: "active"}, raw)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	err = a.Store.View(func(s *State) error {
		beforeUsers, _ := json.Marshal(before.Users)
		afterUsers, _ := json.Marshal(s.Users)
		if !bytes.Equal(beforeUsers, afterUsers) {
			t.Error("user identity/entitlements rewound")
		}
		for name, previous := range before.Docs {
			if name == "routes" || name == "relay_agents" || name == "user_rules" || name == "articles" {
				continue
			}
			left, _ := json.Marshal(previous)
			right, _ := json.Marshal(s.Docs[name])
			if !bytes.Equal(left, right) {
				t.Errorf("preserved collection changed: %s", name)
			}
		}
		route, _ := LoadDoc[Route](s, "routes", "route")
		if route.Name != "current topology" || route.Enabled {
			t.Error("current rule topology not preserved/paused")
		}
		if _, ok := LoadDoc[Route](s, "routes", "unassociated"); !ok {
			t.Error("deleted unassociated route not restored")
		}
		node, _ := LoadDoc[RelayAgent](s, "relay_agents", "node")
		if node.Name != "current node" || node.Enabled || node.TokenHash != "" {
			t.Error("node credentials resurrected")
		}
		if _, ok := LoadDoc[Article](s, "articles", "guide"); !ok {
			t.Error("article not restored")
		}
		for _, record := range ListDocs[BackupRecord](s, "backups") {
			if !record.Protected {
				t.Error("rollback missing/not protected")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRestoreFailureDoesNotCommitPartialState(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	raw := backupSnapshot(t, a, b)
	_ = a.Store.Update(func(s *State) error { s.Settings["maintenance"] = true; return nil })
	// A storage failure creating the mandatory rollback must leave the database
	// unchanged; the rollback path is not bypassed by online safe restore.
	if err := os.WriteFile(filepath.Join(a.Config.DataDir, "backups"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	before := backupSnapshot(t, a, b)
	w := backupRestoreRequest(t, b, &User{Role: "admin", Status: "active"}, raw)
	if w.Code != 409 {
		t.Fatal(w.Body.String())
	}
	left, _ := b.unpack(before)
	right, _ := b.unpack(backupSnapshot(t, a, b))
	x, _ := json.Marshal(left)
	y, _ := json.Marshal(right)
	if !bytes.Equal(x, y) {
		t.Fatal("failed restore mutated state")
	}
}

func TestBackupDeleteProtectsMinimumRollbackAndActiveRestore(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	raw := backupSnapshot(t, a, b)
	var records []BackupRecord
	for range 3 {
		record, err := b.write(raw)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	_ = a.Store.Update(func(s *State) error {
		for _, record := range records {
			_ = SaveDoc(s, "backups", record.ID, record)
		}
		return SaveDoc(s, "backup_plans", "default", BackupPlan{MinCopies: 2, RetentionDays: 30})
	})
	mux := http.NewServeMux()
	b.Register(mux)
	admin := &User{Role: "admin", Status: "active"}
	request := func(id string) *httptest.ResponseRecorder {
		return commerceTestRequest(mux, admin, "DELETE", "/api/admin/backups/"+id, map[string]string{"confirm": "DELETE_LOCAL"})
	}
	b.mu.Lock()
	w := request(records[0].ID)
	b.mu.Unlock()
	if w.Code != 409 {
		t.Fatal("delete allowed during restore")
	}
	w = request(records[0].ID)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(a.Config.DataDir, "backups", records[0].ID+".msb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file not removed")
	}
	if w = request(records[1].ID); w.Code != 409 {
		t.Fatal("minimum copy protection bypassed")
	}
	_ = a.Store.Update(func(s *State) error {
		record := records[1]
		record.Protected = true
		return SaveDoc(s, "backups", record.ID, record)
	})
	if w = request(records[1].ID); w.Code != 409 || !strings.Contains(w.Body.String(), "回滚") {
		t.Fatal("rollback protection bypassed")
	}
	if b.removeLocalBackup("../master.key") == nil {
		t.Fatal("unsafe path accepted")
	}
	_ = a.Store.View(func(s *State) error {
		record, _ := LoadDoc[BackupRecord](s, "backups", records[0].ID)
		if record.Targets["local"] != "removed" {
			t.Error("history missing")
		}
		return nil
	})
}

func TestBackupDeletionDoesNotCountDecryptableButUnrestorableCopies(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	goodRaw := backupSnapshot(t, a, b)
	good, err := b.write(goodRaw)
	if err != nil {
		t.Fatal(err)
	}
	badState, err := b.unpack(goodRaw)
	if err != nil {
		t.Fatal(err)
	}
	_ = SaveDoc(badState, "payment_trades", "bad-trade", "missing-order")
	badRaw, err := b.pack(badState)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Store.Update(func(s *State) error {
		_ = SaveDoc(s, "backups", good.ID, good)
		for range 2 {
			bad, err := b.write(badRaw)
			if err != nil {
				return err
			}
			if b.verifiedLocalBackup(bad) {
				t.Error("unrestorable backup considered valid")
			}
			_ = SaveDoc(s, "backups", bad.ID, bad)
		}
		return SaveDoc(s, "backup_plans", "default", BackupPlan{MinCopies: 2, RetentionDays: 30})
	})
	if err := b.removeLocalBackup(good.ID); err == nil {
		t.Fatal("deleted only fully recoverable backup")
	}
	if !b.verifiedLocalBackup(good) {
		t.Fatal("good backup lost")
	}
}

func TestRestorePreflightIgnoresHeartbeatsAndMeteringButBindsTopology(t *testing.T) {
	s := newState()
	_ = SaveDoc(s, "relay_agents", "node", RelayAgent{ID: "node", Name: "same definition", LastSeen: 1, TokenHash: "token"})
	_ = SaveDoc(s, "executors", "executor", map[string]any{"id": "executor", "lastSeenAt": 1, "status": "active"})
	_ = SaveDoc(s, "user_rules", "user:route", UserRule{ID: "rule", UserID: "user", RouteID: "route", Version: 7, TrafficBytes: 10})
	before, err := restoreScopeFingerprint(s)
	if err != nil {
		t.Fatal(err)
	}
	_ = SaveDoc(s, "relay_agents", "node", RelayAgent{ID: "node", Name: "same definition", LastSeen: 9999, Online: true, BootID: "next epoch", TokenHash: "token"})
	_ = SaveDoc(s, "executors", "executor", map[string]any{"id": "executor", "lastSeenAt": 9999, "status": "active"})
	_ = SaveDoc(s, "user_rules", "user:route", UserRule{ID: "rule", UserID: "user", RouteID: "route", Version: 7, TrafficBytes: 9999, Segments: []RelaySegment{{AckAt: 9999, LastLease: 9999}}})
	after, err := restoreScopeFingerprint(s)
	if err != nil || before != after {
		t.Fatal("heartbeat/traffic invalidates preflight")
	}
	_ = SaveDoc(s, "user_rules", "user:route", UserRule{ID: "rule", UserID: "user", RouteID: "route", Version: 8, TargetHost: "new destination"})
	changed, _ := restoreScopeFingerprint(s)
	if changed == before {
		t.Fatal("topology change not bound")
	}
}

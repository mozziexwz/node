package control

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestOfflineRestoreCreatesNewIsolatedDatabaseAndPreservesCorruptOriginal(t *testing.T) {
	a, _, u := commerceTestApp(t)
	b := NewBackupService(a)
	_ = a.Store.Update(func(s *State) error {
		s.Users[u.ID].BalanceCents = 12345
		s.Sessions["old-session"] = &Session{UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
		if err := SaveDoc(s, "relay_agents", "node", RelayAgent{ID: "node", TokenHash: "secret", EnrollmentHash: "enrollment", Enabled: true}); err != nil {
			return err
		}
		if err := SaveDoc(s, "payment_channels", "pay", PaymentChannel{ID: "pay", Enabled: true}); err != nil {
			return err
		}
		return SaveDoc(s, "tasks", "pending", map[string]any{"id": "pending", "state": "running"})
	})
	raw := backupSnapshot(t, a, b)
	parent := t.TempDir()
	input := filepath.Join(parent, "input.msb")
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(parent, "corrupt-production.db")
	if err := os.WriteFile(original, []byte("intentionally corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := masterKey(a.Config)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(parent, "isolated")
	result, err := OfflineRestore(OfflineRestoreOptions{BackupPath: input, MasterKey: hex.EncodeToString(key), SQLiteOutputDir: output})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Maintenance || result.Database != filepath.Join(output, "msboost.db") {
		t.Fatalf("unexpected result: %+v", result)
	}
	store, err := openStore(Config{DataDir: output})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.View(func(s *State) error {
		if s.Users[u.ID].BalanceCents != 12345 || len(s.Sessions) != 0 || !boolSetting(s, "maintenance") || boolSetting(s, "payali") {
			t.Error("full restore data/isolation mismatch")
		}
		node, _ := LoadDoc[RelayAgent](s, "relay_agents", "node")
		if node.TokenHash != "" || node.EnrollmentHash != "" || node.Enabled {
			t.Error("old agent token resurrected")
		}
		channel, _ := LoadDoc[PaymentChannel](s, "payment_channels", "pay")
		if channel.Enabled {
			t.Error("payment reenabled")
		}
		task, _ := LoadDoc[map[string]any](s, "tasks", "pending")
		if task["state"] != "interrupted" {
			t.Error("old task replayable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(original)
	if err != nil || string(old) != "intentionally corrupt" {
		t.Fatal("corrupt original changed")
	}
	storedKey, err := os.ReadFile(filepath.Join(output, "master.key"))
	if err != nil || !bytes.Equal(key, storedKey) {
		t.Fatal("original master key not preserved")
	}
	if _, err = OfflineRestore(OfflineRestoreOptions{BackupPath: input, MasterKey: hex.EncodeToString(key), SQLiteOutputDir: output}); err == nil {
		t.Fatal("existing target overwritten")
	}
}

func TestOfflineWrongKeyAndInvalidReferencesCreateNoTarget(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	parent := t.TempDir()
	input, output := filepath.Join(parent, "input.msb"), filepath.Join(parent, "output")
	if err := os.WriteFile(input, backupSnapshot(t, a, b), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := OfflineRestore(OfflineRestoreOptions{BackupPath: input, MasterKey: strings.Repeat("ff", 32), SQLiteOutputDir: output})
	if err == nil || !strings.Contains(err.Error(), "解密失败") {
		t.Fatalf("wrong key: %v", err)
	}
	if _, err = os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("wrong key created destination")
	}
	key, err := masterKey(a.Config)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Store.Update(func(s *State) error {
		return SaveDoc(s, "orders", "orphan", Order{ID: "mismatched-id", UserID: "missing-user"})
	})
	if err := os.WriteFile(input, backupSnapshot(t, a, b), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = OfflineRestore(OfflineRestoreOptions{BackupPath: input, MasterKey: hex.EncodeToString(key), SQLiteOutputDir: output})
	if err == nil || !strings.Contains(err.Error(), "身份关联") {
		t.Fatalf("orphan accepted: %v", err)
	}
	if _, err = os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("invalid references created destination")
	}
}

func TestOfflinePostgresNeverAcceptsExistingProductionDatabaseName(t *testing.T) {
	for _, name := range []string{"msboost", "postgres", "msboost_restore_bad-name", "msboost_restore_x\"; DROP DATABASE msboost;--"} {
		if _, err := createRecoveryPostgres("postgres://localhost/postgres", name); err == nil {
			t.Fatalf("unsafe database name accepted: %s", name)
		}
	}
}

func TestOfflineRestorePostgresIntegration(t *testing.T) {
	adminURL := os.Getenv("MSBOOST_TEST_POSTGRES_URL")
	if adminURL == "" {
		t.Skip("set MSBOOST_TEST_POSTGRES_URL to a disposable PostgreSQL instance")
	}
	a, _, u := commerceTestApp(t)
	b := NewBackupService(a)
	_ = a.Store.Update(func(s *State) error { s.Users[u.ID].BalanceCents = 87654; return nil })
	input := filepath.Join(t.TempDir(), "source.msb")
	if err := os.WriteFile(input, backupSnapshot(t, a, b), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := masterKey(a.Config)
	if err != nil {
		t.Fatal(err)
	}
	// No DROP DATABASE exists in the recovery implementation or tests. CI owns
	// the disposable postgres service and removes the whole test container.
	name := "msboost_restore_test_" + hex.EncodeToString([]byte(ID())[:10])
	options := OfflineRestoreOptions{BackupPath: input, MasterKey: hex.EncodeToString(key), PostgresAdminURL: adminURL, PostgresNewDatabase: name}
	result, err := OfflineRestore(options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Database != name || !result.Maintenance {
		t.Fatalf("unexpected restore result: %+v", result)
	}
	// A second attempt must fail at CREATE DATABASE, before any restored state
	// can be overwritten, even when it receives the same valid backup and key.
	if _, err = OfflineRestore(options); err == nil {
		t.Fatal("existing PostgreSQL recovery target overwritten")
	}
}

func TestOfflineRestorePreservesFinancialAndMeteringClosureIncludingDeletedUsers(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	// Historical data may legitimately outlive a deleted account/node/route.
	// Restore keeps those IDs, not a newly-created replacement identity.
	_ = a.Store.Update(func(s *State) error {
		rows := []struct {
			collection, id string
			value          any
		}{
			{"orders", "order", Order{ID: "order", UserID: "deleted-user", State: "paid", AmountCents: 100}},
			{"ledger", "entry", LedgerEntry{ID: "entry", UserID: "deleted-user", AmountCents: 100, BalanceAfter: 100}},
			{"cards", "card", BalanceCard{ID: "card", Status: "used", UsedBy: "deleted-user", AmountCents: 100}},
			{"order_requests", "request", commerceIdempotency{ObjectID: "order", Fingerprint: "hash"}},
			{"redeem_requests", "request", commerceIdempotency{ObjectID: "card", Fingerprint: "hash"}},
			{"payment_trades", "provider:trade", "order"},
			{"entitlement_versions", "deleted-user", int64(7)},
			{"relay_rule_archive", "archived-rule", UserRule{ID: "archived-rule", UserID: "deleted-user", RouteID: "deleted-route", TrafficBytes: 555}},
			{"traffic_cursors", "deleted-node:archived-rule:epoch-1234567890", TrafficCursor{Sequence: 10, InputBytes: 555, OutputBytes: 0, Remainder: 9}},
			{"traffic_months", "deleted-user:2026-09", int64(555)},
			{"future_collection", "preserve", map[string]any{"associatedUser": "deleted-user", "value": 123}},
		}
		for _, row := range rows {
			if err := SaveDoc(s, row.collection, row.id, row.value); err != nil {
				return err
			}
		}
		return nil
	})
	var before *State
	_ = a.Store.View(func(s *State) error { before, _ = cloneState(s); return nil })
	parent := t.TempDir()
	input, output := filepath.Join(parent, "backup.msb"), filepath.Join(parent, "restore")
	if err := os.WriteFile(input, backupSnapshot(t, a, b), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := masterKey(a.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OfflineRestore(OfflineRestoreOptions{BackupPath: input, MasterKey: hex.EncodeToString(key), SQLiteOutputDir: output}); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(Config{DataDir: output})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.View(func(s *State) error {
		if s.Users["deleted-user"] != nil {
			t.Error("historical user was fabricated")
		}
		for name, previous := range before.Docs {
			left, _ := json.Marshal(previous)
			right, _ := json.Marshal(s.Docs[name])
			if !bytes.Equal(left, right) {
				t.Errorf("offline financial/traffic collection changed: %s", name)
			}
		}
		return nil
	})
}

func TestOfflineRestoreRealBalanceAndFreeOrders(t *testing.T) {
	for _, price := range []int64{0, 100} {
		t.Run(fmt.Sprint(price), func(t *testing.T) {
			a, mux, user := commerceTestApp(t)
			_ = a.Store.Update(func(s *State) error {
				s.Users[user.ID].BalanceCents = 1000
				return SaveDoc(s, "plans", "plan", Plan{ID: "plan", Name: "restore regression", PriceCents: price, Days: 1, TrafficBytes: 1000000, RateMbps: 5, Enabled: true})
			})
			response := commerceTestRequest(mux, user, "POST", "/api/orders", map[string]any{"planId": "plan", "channelId": "balance", "requestId": "backup-balance-regression", "confirmReplace": true})
			if response.Code != 201 {
				t.Fatalf("purchase: %d %s", response.Code, response.Body.String())
			}
			b := NewBackupService(a)
			parent := t.TempDir()
			input := filepath.Join(parent, "balance.msb")
			if err := os.WriteFile(input, backupSnapshot(t, a, b), 0600); err != nil {
				t.Fatal(err)
			}
			key, err := masterKey(a.Config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = OfflineRestore(OfflineRestoreOptions{BackupPath: input, MasterKey: hex.EncodeToString(key), SQLiteOutputDir: filepath.Join(parent, "new")}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecoveryPostgresRejectsRoutingOverridesBeforeOpeningDatabase(t *testing.T) {
	name := "msboost_restore_regression"
	for _, suffix := range []string{"dbname=msboost", "database=msboost", "%64bname=msboost", "service=production", "host=other", "sslmode=disable&dbname=msboost", "sslmode=disable&sslmode=require"} {
		if _, err := recoveryPostgresTarget("postgres://user:password@127.0.0.1/postgres?"+suffix, name); err == nil {
			t.Errorf("routing override accepted: %s", suffix)
		}
	}
	target, err := recoveryPostgresTarget("postgres://user:password@127.0.0.1/postgres?sslmode=disable&connect_timeout=5", name)
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(target)
	if err != nil || config.Database != name {
		t.Fatal("effective pgx target is not the new database")
	}
}

package control

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func backupPauseTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openStore(Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestBackupPauseOwnerPersistenceAndReadOnlyStatus(t *testing.T) {
	store := backupPauseTestStore(t)
	token, now := strings.Repeat("ab", 32), time.Now().UnixMilli()
	result, err := backupPauseAcquire(store, token, now)
	if err != nil || !result.GateActive || !result.CanPauseControl {
		t.Fatal(result, err)
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte(token)) || bytes.Contains(encoded, []byte(tokenHash(token))) {
		t.Fatal("owner credential leaked")
	}
	var raw string
	var revision, afterRevision int64
	if err = store.db.QueryRow("SELECT payload,revision FROM control_state WHERE id=1").Scan(&raw, &revision); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, token) || !strings.Contains(raw, tokenHash(token)) {
		t.Fatal("gate did not use a hash")
	}
	status, err := backupPauseStatus(store, now+24*60*60*1000)
	if err != nil || !status.GateActive || status.CanPauseControl {
		t.Fatal(status, err)
	}
	if err = store.db.QueryRow("SELECT revision FROM control_state WHERE id=1").Scan(&afterRevision); err != nil || revision != afterRevision {
		t.Fatal("status wrote state", err)
	}
	called := false
	if err = store.Update(func(s *State) error { called = true; delete(s.Docs, backupPauseCollection); return nil }); !errors.Is(err, ErrBackupPauseActive) || called {
		t.Fatal("write bypassed persistent gate", err)
	}
	if _, err = backupPauseAcquire(store, token, now+24*60*60*1000); err != nil {
		t.Fatal("same owner retry failed", err)
	}
	if _, err = backupPauseAcquire(store, strings.Repeat("cd", 32), now); !errors.Is(err, errBackupPauseOwner) {
		t.Fatal("other owner acquired gate", err)
	}
	if _, err = backupPauseRelease(store, strings.Repeat("cd", 32)); !errors.Is(err, errBackupPauseOwner) {
		t.Fatal("other owner released gate", err)
	}
	if result, err = backupPauseRelease(store, token); err != nil || !result.Released {
		t.Fatal(result, err)
	}
	if _, err = backupPauseRelease(store, token); err != nil {
		t.Fatal("uncertain release cannot retry", err)
	}
	if err = store.Update(func(s *State) error { s.Settings["afterBackup"] = true; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestBackupPauseMalformedMarkersFailClosed(t *testing.T) {
	for _, test := range []struct{ key, raw string }{
		{"default", "null"}, {"default", `{}`}, {"unexpected", `{}`},
		{"default", `{"version":2,"tokenHash":"` + strings.Repeat("ab", 32) + `","acquiredAt":1}`},
		{"default", `{"version":1,"tokenHash":"bad","acquiredAt":1}`},
		{"default", `{"version":1,"tokenHash":"` + strings.Repeat("ab", 32) + `","acquiredAt":1,"mode":"force"}`},
		{"default", `{"version":1,"tokenHash":"` + strings.Repeat("ab", 32) + `","acquiredAt":1,"mode":"strict","acceptedRisks":["offline_unsupported"]}`},
		{"default", `{"version":1,"tokenHash":"` + strings.Repeat("ab", 32) + `","acquiredAt":1,"mode":"maintenance","acceptedRisks":["recovery_required"]}`},
		{"default", `{"version":1,"tokenHash":"` + strings.Repeat("ab", 32) + `","acquiredAt":1,"mode":"maintenance","acceptedRisks":["offline_unsupported","offline_unsupported"]}`},
	} {
		t.Run(test.key+test.raw[:min(10, len(test.raw))], func(t *testing.T) {
			store := backupPauseTestStore(t)
			if err := store.Update(func(s *State) error {
				s.Docs[backupPauseCollection] = map[string]json.RawMessage{test.key: json.RawMessage(test.raw)}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Update(func(s *State) error { t.Error("callback executed under corrupt gate"); return nil }); !errors.Is(err, ErrBackupPauseActive) {
				t.Fatal(err)
			}
			if _, err := backupPauseAcquire(store, strings.Repeat("ab", 32), time.Now().UnixMilli()); !errors.Is(err, errBackupPauseCorrupt) {
				t.Fatal(err)
			}
			if _, err := backupPauseAcquireMode(store, strings.Repeat("ab", 32), time.Now().UnixMilli(), "maintenance"); !errors.Is(err, errBackupPauseCorrupt) {
				t.Fatal("maintenance bypassed malformed gate", err)
			}
			if _, err := backupPauseRelease(store, strings.Repeat("ab", 32)); !errors.Is(err, errBackupPauseCorrupt) {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupPauseChecksLatestStateInWriterTransaction(t *testing.T) {
	for _, mode := range []string{"strict", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			config := Config{DataDir: t.TempDir()}
			store, err := openStore(config)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			// A second Store/connection exercises SQLite's inter-connection writer
			// barrier as well as the in-process mutex used by production transactions.
			other, err := openStore(config)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			entered, finish := make(chan struct{}), make(chan struct{})
			writeDone := make(chan error, 1)
			go func() {
				writeDone <- store.Update(func(s *State) error {
					close(entered)
					<-finish
					s.Docs["tasks"] = map[string]json.RawMessage{"busy": json.RawMessage(`{"id":"busy","state":"running"}`)}
					return nil
				})
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("writer did not enter")
			}
			acquireDone := make(chan error, 1)
			go func() {
				_, err := backupPauseAcquireMode(other, strings.Repeat("ab", 32), time.Now().UnixMilli(), mode)
				acquireDone <- err
			}()
			select {
			case err := <-acquireDone:
				t.Errorf("acquire escaped writer lock: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			close(finish)
			if err := <-writeDone; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-acquireDone:
				if !errors.Is(err, errBackupPauseBlocked) {
					t.Fatal("stale preflight permitted stop", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("acquire stuck")
			}
			if err := store.View(func(s *State) error {
				if backupPauseGatePresent(s) {
					t.Error("failed preflight left a gate")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupPauseCommitFailuresDoNotPretendUnlocked(t *testing.T) {
	for _, mode := range []string{"strict", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			store := backupPauseTestStore(t)
			token := strings.Repeat("ab", 32)
			if _, err := store.db.Exec(`CREATE TRIGGER fail_gate_write BEFORE UPDATE ON control_state BEGIN SELECT RAISE(ABORT, 'simulated disk failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := backupPauseAcquireMode(store, token, time.Now().UnixMilli(), mode); err == nil {
				t.Fatal("failed acquire reported success")
			}
			if err := store.View(func(s *State) error {
				if backupPauseGatePresent(s) {
					t.Error("uncommitted gate survived")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`DROP TRIGGER fail_gate_write`); err != nil {
				t.Fatal(err)
			}
			if _, err := backupPauseAcquireMode(store, token, time.Now().UnixMilli(), mode); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`CREATE TRIGGER fail_gate_write BEFORE UPDATE ON control_state BEGIN SELECT RAISE(ABORT, 'simulated disk failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := backupPauseRelease(store, token); err == nil {
				t.Fatal("failed release reported success")
			}
			if err := store.Update(func(s *State) error { t.Error("failed release reopened writes"); return nil }); !errors.Is(err, ErrBackupPauseActive) {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupPauseHTTPRejectsAllAPIWorkBeforeHandler(t *testing.T) {
	store := backupPauseTestStore(t)
	if _, err := backupPauseAcquire(store, strings.Repeat("ab", 32), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	app := &App{Store: store}
	handler := app.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("handler executed during backup gate") }))
	for _, method := range []string{"GET", "POST", "DELETE"} {
		for _, path := range []string{"/api/executor/next", "/api/payment/notify", "/api/relay-agent/v2/sync", "/api/health"} {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
			if rr.Code != 503 || rr.Header().Get("Retry-After") != "30" {
				t.Fatal(method, path, rr.Code)
			}
		}
	}
}

func TestBackupPauseTokenFrameAndRestoreIsolation(t *testing.T) {
	token := strings.Repeat("ab", 32)
	for _, invalid := range []string{"", token, token + "\r\n", token + "\nextra", strings.ToUpper(token) + "\n", strings.Repeat("z", 64) + "\n"} {
		if _, err := backupPauseReadToken(strings.NewReader(invalid)); err == nil {
			t.Fatal("invalid owner frame accepted")
		}
	}
	if value, err := backupPauseReadToken(strings.NewReader(token + "\n")); err != nil || value != token {
		t.Fatal(err)
	}
	s := newState()
	_ = SaveDoc(s, backupPauseCollection, "default", backupPauseGate{Version: 1, TokenHash: tokenHash(token), AcquiredAt: 1})
	_ = SaveDoc(s, "backup_operations", "old", map[string]any{"state": "running"})
	_ = SaveDoc(s, "relay_v2_control", "default", RelayV2Control{Epoch: "old", RecoveryRequired: true})
	if err := validateRestoreIdle(s); !errors.Is(err, ErrBackupPauseActive) {
		t.Fatal("safe restore could clear live gate", err)
	}
	delete(s.Docs, backupPauseCollection)
	s.Settings["maintenance"] = true
	if err := validateRestoreIdle(s); err == nil {
		t.Fatal("safe restore could clear another process's live backup activity")
	}
	_ = SaveDoc(s, backupPauseCollection, "default", backupPauseGate{Version: 1, TokenHash: tokenHash(token), AcquiredAt: 1})
	if err := isolateRestoredState(s, true); err != nil {
		t.Fatal(err)
	}
	if backupPauseGatePresent(s) || len(s.Docs["backup_operations"]) != 0 || !restoredRelayRecoveryRequired(s) || !boolSetting(s, "maintenance") {
		t.Fatal("restore isolation confused local gate and relay recovery")
	}
}

func TestBackupPausePostgresIntegration(t *testing.T) {
	adminURL := os.Getenv("MSBOOST_TEST_POSTGRES_URL")
	if adminURL == "" {
		t.Skip("isolated PostgreSQL integration URL not set")
	}
	name := "msboost_restore_pause_" + hex.EncodeToString([]byte(ID()[:10]))
	target, err := createRecoveryPostgres(adminURL, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		db, e := sql.Open("pgx", adminURL)
		if e == nil {
			defer db.Close()
			_, _ = db.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`)
		}
	}()
	db, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	helper := &Store{db: db, dialect: "postgres"}
	token := strings.Repeat("ab", 32)
	if _, err = backupPauseAcquire(helper, token, time.Now().UnixMilli()); err == nil {
		t.Fatal("missing schema accepted")
	}
	var tables int
	if err = db.QueryRow("SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("helper initialized missing schema", err)
	}
	server, err := openStore(Config{DatabaseURL: target})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- server.Update(func(s *State) error {
			close(entered)
			<-finish
			s.Docs["backup_operations"] = map[string]json.RawMessage{"active": json.RawMessage(`{"id":"active"}`)}
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not begin")
	}
	acquired := make(chan error, 1)
	go func() { _, err := backupPauseAcquire(helper, token, time.Now().UnixMilli()); acquired <- err }()
	select {
	case err := <-acquired:
		t.Errorf("FOR UPDATE did not serialize helper: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = <-acquired; !errors.Is(err, errBackupPauseBlocked) {
		t.Fatal("helper checked stale state", err)
	}
	if err = server.Update(func(s *State) error { delete(s.Docs, "backup_operations"); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = backupPauseAcquire(helper, token, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err = server.Update(func(s *State) error { t.Error("server bypassed CLI gate"); return nil }); !errors.Is(err, ErrBackupPauseActive) {
		t.Fatal(err)
	}
	if _, err = backupPauseRelease(helper, token); err != nil {
		t.Fatal(err)
	}
	if err = server.Update(func(s *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

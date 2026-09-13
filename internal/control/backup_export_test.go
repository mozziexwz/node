package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportStateBackupRoundTripAndIsolation(t *testing.T) {
	a, _, user := commerceTestApp(t)
	var raw []byte
	if err := a.Store.View(func(s *State) error { var err error; raw, err = json.Marshal(s); return err }); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := exportStateBackup(raw, a.aead, &output); err != nil {
		t.Fatal(err)
	}
	state, err := NewBackupService(a).unpack(output.Bytes())
	if err != nil || state.Users[user.ID] == nil {
		t.Fatal("encrypted export cannot recover original user", err)
	}
	key, _ := masterKey(a.Config)
	directory := t.TempDir()
	input := filepath.Join(directory, "state.msb")
	if err = os.WriteFile(input, output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = OfflineRestore(OfflineRestoreOptions{BackupPath: input, MasterKey: hex.EncodeToString(key), SQLiteOutputDir: filepath.Join(directory, "new")}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{`{}`, `{"users":{},"sessions":{},"settings":{}}`, `broken`} {
		output.Reset()
		if err = exportStateBackup([]byte(invalid), a.aead, &output); err == nil || output.Len() != 0 {
			t.Fatal("invalid state emitted a usable backup")
		}
	}
}

func TestBackupExportRequiresOriginalKeyAndPostgres(t *testing.T) {
	if _, err := backupExportCipher("", ""); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := backupExportCipher(strings.Repeat("ab", 32), "key"); err == nil {
		t.Fatal("ambiguous key accepted")
	}
	file := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(file, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := backupExportCipher("", file); err == nil {
		t.Fatal("short key accepted")
	}
	var output bytes.Buffer
	if err := ExportPostgresBackup("sqlite://missing.db", strings.Repeat("ab", 32), "", &output); err == nil {
		t.Fatal("unsupported database accepted")
	}
}

func TestBackupExportPostgresReadOnlyIntegration(t *testing.T) {
	admin := os.Getenv("MSBOOST_TEST_POSTGRES_URL")
	if admin == "" {
		t.Skip("isolated PostgreSQL integration URL not set")
	}
	name := "msboost_restore_export_" + strings.ReplaceAll(ID(), "-", "")[:16]
	target, err := createRecoveryPostgres(admin, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		db, e := sql.Open("pgx", admin)
		if e == nil {
			defer db.Close()
			_, _ = db.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`)
		}
	}()
	key := strings.Repeat("ab", 32)
	var output bytes.Buffer
	if err = ExportPostgresBackup(target, key, "", &output); err == nil {
		t.Fatal("missing database schema silently created")
	}
	db, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRowContext(context.Background(), "SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed export changed schema", err, count)
	}
	store, err := openStore(Config{DatabaseURL: target})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = ExportPostgresBackup(target, key, "", &output); err != nil {
		t.Fatal(err)
	}
	var revision int
	if err = db.QueryRow("SELECT revision FROM control_state WHERE id=1").Scan(&revision); err != nil || revision != 0 {
		t.Fatal("export changed original revision", err)
	}
}

//go:build linux

package control

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

// Test-only child used by the guarded real Docker cycle. It initializes the
// actual production flock as the official application UID and commits a real
// PostgreSQL activity marker, then remains alive until the isolated container
// is killed. It is never included in server/restore production binaries.
func TestBackupActivityComposeFixture(t *testing.T) {
	if os.Getenv("MSBOOST_CI_ACTIVITY_FIXTURE") != "hold" {
		t.Skip("dedicated Docker integration child only")
	}
	if os.Geteuid() != 10001 || os.Getenv("DATA_DIR") != "/app/data" || os.Getenv("DATABASE_HOST") != "database" || os.Getenv("DATABASE_NAME") != "msboost" || os.Getenv("DATABASE_USER") != "msboost" || os.Getenv("DATABASE_SSLMODE") != "disable" {
		t.Fatal("refusing fixture outside the explicitly scoped CI Compose service")
	}
	id := os.Getenv("MSBOOST_CI_ACTIVITY_ID")
	if !validBackupID(id) || len(id) < 20 {
		t.Fatal("missing unique CI fixture operation identity")
	}
	lock, err := acquireBackupActivityLock("/app/data", true)
	if err != nil {
		t.Fatal("cannot acquire original application lock")
	}
	defer lock.Close()
	u := url.URL{Scheme: "postgres", Host: "database:5432", Path: "/msboost", User: url.UserPassword("msboost", os.Getenv("POSTGRES_PASSWORD")), RawQuery: "sslmode=disable"}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal("cannot open isolated PostgreSQL")
	}
	defer db.Close()
	store := &Store{db: db, dialect: "postgres"}
	operation := BackupOperation{ID: id, Kind: "site-backup", StartedAt: time.Now().UnixMilli(), LockIdentity: lock.Identity, LockDevice: lock.Device, LockInode: lock.Inode}
	if err := store.Update(func(s *State) error { return beginBackupOperation(s, operation) }); err != nil {
		t.Fatal("cannot persist isolated activity marker")
	}
	fmt.Fprintln(os.Stdout, "CI_BACKUP_ACTIVITY_OWNER_READY")
	// No TTL logic is under test: the harness kills this process explicitly,
	// and verifies Linux released the SAME inode before allowing reconciliation.
	time.Sleep(5 * time.Minute)
	t.Fatal("CI fixture controller did not finish within its bounded lifetime")
}

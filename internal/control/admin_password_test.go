package control

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestAdminPasswordInput(t *testing.T) {
	for _, password := range []string{strings.Repeat("x", 12), strings.Repeat("x", 72), strings.Repeat("密", 8), " space!$'\"password "} {
		payload := "ADMIN@example.com\n" + password + "\n" + password + "\nRESET admin@example.com\n"
		email, hash, err := readAdminPasswordInput(strings.NewReader(payload))
		if err != nil || email != "admin@example.com" || bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
			t.Fatalf("valid stdin frame failed: %v", err)
		}
	}
	for _, payload := range []string{
		"", "admin@example.com\n", "admin@example.com\nshort\nshort\nRESET admin@example.com\n",
		"admin@example.com\n" + strings.Repeat("a", 73) + "\n" + strings.Repeat("a", 73) + "\nRESET admin@example.com\n",
		"admin@example.com\nsecret-password-1\nsecret-password-2\nRESET admin@example.com\n",
		"admin@example.com\nsecret-password-1\nsecret-password-1\nRESET other@example.com\n",
		"admin@example.com\nsecret-password-1\nsecret-password-1\nCANCEL\n",
		"Admin <admin@example.com>\nsecret-password-1\nsecret-password-1\nRESET Admin <admin@example.com>\n",
		"admin@example.com\nsecret-password-1\nsecret-password-1\nRESET admin@example.com",
		"admin@example.com\nsecret-password-1\nsecret-password-1\nRESET admin@example.com\nextra\n",
		"admin@example.com\nsecret\x00password-1\nsecret\x00password-1\nRESET admin@example.com\n",
		strings.Repeat("x", 1025),
	} {
		_, _, err := readAdminPasswordInput(strings.NewReader(payload))
		if err == nil || strings.Contains(err.Error(), "secret-password") {
			t.Fatal("invalid frame was accepted or leaked a credential")
		}
	}
	if _, _, err := readAdminPasswordInput(errorAdminPasswordReader{}); err == nil || strings.Contains(err.Error(), "reader-secret") {
		t.Fatal("reader failure was swallowed or echoed")
	}
}

type errorAdminPasswordReader struct{}

func (errorAdminPasswordReader) Read([]byte) (int, error) { return 0, errors.New("reader-secret") }

func adminPasswordFixture(t *testing.T) (*Store, Config) {
	t.Helper()
	c := Config{DataDir: t.TempDir()}
	store, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	err = store.Update(func(s *State) error {
		s.Users["admin"] = &User{ID: "admin", Email: "admin@example.com", Role: "admin", Status: "active", PasswordHash: "old-hash", BalanceCents: 123, TrafficUsed: 40}
		s.Users["admin2"] = &User{ID: "admin2", Email: "other@example.com", Role: "admin", PasswordHash: "other-hash"}
		s.Users["member"] = &User{ID: "member", Email: "member@example.com", Role: "member", PasswordHash: "member-hash"}
		s.Sessions["admin-session"] = &Session{UserID: "admin"}
		s.Sessions["member-session"] = &Session{UserID: "member"}
		s.Sessions["other-session"] = &Session{UserID: "admin2"}
		s.Settings["maintenance"] = true
		s.Settings["payment-enabled"] = true
		for _, challenge := range []emailChallenge{
			{ID: "owner", Owner: "admin", Email: "prior@example.com", Purpose: "password_reset"},
			{ID: "email", Owner: "old-id", Email: "ADMIN@example.com", Purpose: "password_reset"},
			{ID: "unrelated", Owner: "admin", Email: "admin@example.com", Purpose: "verify"},
			{ID: "member", Owner: "member", Email: "member@example.com", Purpose: "password_reset"},
		} {
			if err := SaveDoc(s, "email_challenges", challenge.ID, challenge); err != nil {
				return err
			}
		}
		return SaveDoc(s, "untouched-business", "example", map[string]any{"balance": 100, "rules": "keep", "encrypted": "preserve"})
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, c
}

func TestChangeExistingAdminPasswordScope(t *testing.T) {
	store, _ := adminPasswordFixture(t)
	password := "replacement-password-123"
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	var before []byte
	_ = store.View(func(s *State) error { before, _ = json.Marshal(s); return nil })
	if err := changeExistingAdminPassword(store, "admin@example.com", hash); err != nil {
		t.Fatal(err)
	}
	if err := store.View(func(s *State) error {
		if bcrypt.CompareHashAndPassword([]byte(s.Users["admin"].PasswordHash), []byte(password)) != nil {
			t.Fatal("administrator hash not changed")
		}
		if _, ok := s.Sessions["admin-session"]; ok {
			t.Fatal("old administrator session survived")
		}
		if _, ok := s.Docs["email_challenges"]["owner"]; ok {
			t.Fatal("owner recovery survived")
		}
		if _, ok := s.Docs["email_challenges"]["email"]; ok {
			t.Fatal("email recovery survived")
		}
		current, _ := json.Marshal(s)
		if bytes.Contains(current, []byte(password)) {
			t.Fatal("plaintext password persisted")
		}
		// Restore the deliberately changed fields; every other persisted value
		// must remain byte-for-byte equivalent, including maintenance/billing.
		var previous State
		_ = json.Unmarshal(before, &previous)
		s.Users["admin"].PasswordHash = previous.Users["admin"].PasswordHash
		s.Sessions["admin-session"] = previous.Sessions["admin-session"]
		s.Docs["email_challenges"]["owner"] = previous.Docs["email_challenges"]["owner"]
		s.Docs["email_challenges"]["email"] = previous.Docs["email_challenges"]["email"]
		delete(s.Docs, "audit")
		after, _ := json.Marshal(s)
		if !bytes.Equal(before, after) {
			t.Fatal("unrelated state changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdminPasswordRejectsNonAdminAndMissing(t *testing.T) {
	for _, email := range []string{"member@example.com", "missing@example.com"} {
		t.Run(email, func(t *testing.T) {
			store, _ := adminPasswordFixture(t)
			var before, after []byte
			_ = store.View(func(s *State) error { before, _ = json.Marshal(s); return nil })
			if err := changeExistingAdminPassword(store, email, []byte("hash")); err == nil {
				t.Fatal("non-admin target accepted")
			}
			_ = store.View(func(s *State) error { after, _ = json.Marshal(s); return nil })
			if !bytes.Equal(before, after) {
				t.Fatal("rejected request mutated state")
			}
		})
	}
}

func TestAdminPasswordExistingStoreDoesNotCreateSchema(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db, dialect: "sqlite"}
	if err := changeExistingAdminPassword(store, "admin@example.com", []byte("hash")); err == nil {
		t.Fatal("empty database accepted")
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='control_state'").Scan(&count); err != nil || count != 0 {
		t.Fatal("missing schema was initialized")
	}
}

func TestAdminPasswordConcurrentStateWriter(t *testing.T) {
	server, config := adminPasswordFixture(t)
	helper, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	writing, release, writerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		writerDone <- server.Update(func(s *State) error {
			s.Users["admin"].BalanceCents += 700
			s.Users["admin"].TrafficUsed += 9
			close(writing)
			<-release
			return SaveDoc(s, "concurrent", "payment", "committed")
		})
	}()
	<-writing
	passwordDone := make(chan error, 1)
	go func() {
		passwordDone <- changeExistingAdminPassword(helper, "admin@example.com", []byte("replacement-hash"))
	}()
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-passwordDone; err != nil {
		t.Fatal(err)
	}
	if err := server.View(func(s *State) error {
		u := s.Users["admin"]
		if u.BalanceCents != 823 || u.TrafficUsed != 49 || u.PasswordHash != "replacement-hash" {
			t.Fatal("concurrent write overwritten")
		}
		if value, _ := LoadDoc[string](s, "concurrent", "payment"); value != "committed" {
			t.Fatal("concurrent record overwritten")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdminPasswordBootstrapDoesNotOverwriteExisting(t *testing.T) {
	store, config := adminPasswordFixture(t)
	if err := changeExistingAdminPassword(store, "admin@example.com", []byte("new-local-hash")); err != nil {
		t.Fatal(err)
	}
	config.AdminEmail, config.AdminPassword = "admin@example.com", "old-env-password-123"
	app := &App{Store: store, Config: config}
	if err := app.initialize(); err != nil {
		t.Fatal(err)
	}
	_ = store.View(func(s *State) error {
		if s.Users["admin"].PasswordHash != "new-local-hash" {
			t.Fatal("old .env bootstrap password overwrote administrator")
		}
		return nil
	})
}

func TestAdminPasswordPlatformGuard(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		if err := ChangeLocalAdminPassword("postgres://irrelevant", io.LimitReader(strings.NewReader(""), 0)); err == nil {
			t.Fatal("non-Linux administrator command accepted")
		}
	}
}

func TestAdminPasswordPostgresIntegration(t *testing.T) {
	adminURL := os.Getenv("MSBOOST_TEST_POSTGRES_URL")
	if adminURL == "" {
		t.Skip("isolated PostgreSQL integration URL not set")
	}
	// Only a fresh randomized msboost_restore_* test database is created. Never
	// operate on the database selected by DATABASE_URL/production environment.
	name := "msboost_restore_admin_" + hex.EncodeToString([]byte(ID()[:10]))
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
	if err := changeExistingAdminPassword(helper, "admin@example.com", []byte("hash")); err == nil {
		t.Fatal("missing schema was accepted")
	}
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&tables); err != nil || tables != 0 {
		t.Fatal("administrator command created missing schema")
	}
	server, err := openStore(Config{DatabaseURL: target})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if err := server.Update(func(s *State) error {
		s.Users["admin"] = &User{ID: "admin", Email: "admin@example.com", Role: "admin", PasswordHash: "old"}
		s.Sessions["old-session"] = &Session{UserID: "admin"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	password := "Synthetic-Postgres-Password-123!"
	_, hash, err := readAdminPasswordInput(strings.NewReader("admin@example.com\n" + password + "\n" + password + "\nRESET admin@example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	start, results := make(chan struct{}), make(chan error, 2)
	go func() {
		<-start
		for i := 0; i < 20; i++ {
			if err := server.Update(func(s *State) error { s.Users["admin"].BalanceCents++; return nil }); err != nil {
				results <- err
				return
			}
		}
		results <- nil
	}()
	go func() {
		<-start
		for i := 0; i < 20; i++ {
			if err := changeExistingAdminPassword(helper, "admin@example.com", hash); err != nil {
				results <- err
				return
			}
		}
		results <- nil
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if err := server.View(func(s *State) error {
		if s.Users["admin"].BalanceCents != 20 || bcrypt.CompareHashAndPassword([]byte(s.Users["admin"].PasswordHash), []byte(password)) != nil {
			t.Fatal("PostgreSQL FOR UPDATE failed to preserve concurrent writes")
		}
		if len(s.Sessions) != 0 {
			t.Fatal("administrator session survived")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Run("LinuxRootExistingDatabaseEntry", func(t *testing.T) {
		if runtime.GOOS != "linux" || os.Geteuid() != 0 {
			t.Skip("actual local administrator entry requires Linux root")
		}
		if err := server.Update(func(s *State) error {
			s.Sessions["entry-old-session"] = &Session{UserID: "admin"}
			return nil
		}); err != nil {
			t.Fatal("could not seed the isolated administrator session")
		}
		// Exercise the public root-only entry and a separate existing-DB
		// connection. The private stdin frame must never enter a log or argv.
		entryPassword := "Synthetic-Local-Entry-" + ID()
		frame := "admin@example.com\n" + entryPassword + "\n" + entryPassword + "\nRESET admin@example.com\n"
		if err := ChangeLocalAdminPassword(target, strings.NewReader(frame)); err != nil {
			t.Fatal("Linux root administrator entry failed")
		}
		if err := server.View(func(s *State) error {
			user := s.Users["admin"]
			if user == nil || user.BalanceCents != 20 || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(entryPassword)) != nil {
				t.Fatal("local entry changed the balance or did not persist the new hash")
			}
			if len(s.Sessions) != 0 {
				t.Fatal("local entry did not revoke the existing administrator session")
			}
			encoded, err := json.Marshal(s)
			if err != nil || bytes.Contains(encoded, []byte(entryPassword)) {
				t.Fatal("local entry persisted plaintext or invalid state")
			}
			return nil
		}); err != nil {
			t.Fatal("could not verify the isolated administrator state")
		}
	})
}

package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// State is the transaction boundary shared by identity, billing and execution.
// Callbacks must not retain pointers to it or perform external network work.
type State struct {
	Users    map[string]*User                      `json:"users"`
	Sessions map[string]*Session                   `json:"sessions"`
	Settings map[string]any                        `json:"settings"`
	Docs     map[string]map[string]json.RawMessage `json:"docs"`
}

type Store struct {
	db      *sql.DB
	dialect string
	mu      sync.Mutex
}

func newState() *State {
	return &State{Users: map[string]*User{}, Sessions: map[string]*Session{}, Settings: map[string]any{}, Docs: map[string]map[string]json.RawMessage{}}
}

func openStore(c Config) (*Store, error) {
	driver, source, dialect := "sqlite", c.DatabaseURL, "sqlite"
	if strings.HasPrefix(source, "postgres://") || strings.HasPrefix(source, "postgresql://") {
		driver, dialect = "pgx", "postgres"
	} else {
		if source == "" {
			if err := os.MkdirAll(c.DataDir, 0700); err != nil {
				return nil, err
			}
			source = filepath.Join(c.DataDir, "msboost.db")
		}
		source = strings.TrimPrefix(source, "sqlite://")
	}
	db, err := sql.Open(driver, source)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, dialect: dialect}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if dialect == "sqlite" {
		for _, query := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=15000", "PRAGMA foreign_keys=ON", "PRAGMA synchronous=FULL"} {
			if _, err = db.ExecContext(ctx, query); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	if _, err = db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS control_state (id INTEGER PRIMARY KEY, payload TEXT NOT NULL, revision BIGINT NOT NULL DEFAULT 0)"); err != nil {
		db.Close()
		return nil, err
	}
	initial, _ := json.Marshal(newState())
	query := "INSERT INTO control_state(id,payload,revision) VALUES(1,?,0) ON CONFLICT(id) DO NOTHING"
	if dialect == "postgres" {
		query = "INSERT INTO control_state(id,payload,revision) VALUES(1,$1,0) ON CONFLICT(id) DO NOTHING"
	}
	if _, err = db.ExecContext(ctx, query, string(initial)); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) View(fn func(*State) error) error   { return s.transaction(false, fn) }
func (s *Store) Update(fn func(*State) error) error { return s.transaction(true, fn) }

func (s *Store) transaction(write bool, fn func(*State) error) (err error) {
	// A single connection prevents local read/modify/write races. BEGIN IMMEDIATE
	// (SQLite) and FOR UPDATE (Postgres) also serialize independent processes.
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	begin := "BEGIN"
	if write && s.dialect == "sqlite" {
		begin = "BEGIN IMMEDIATE"
	}
	if _, err = conn.ExecContext(ctx, begin); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	query := "SELECT payload FROM control_state WHERE id=1"
	if write && s.dialect == "postgres" {
		query += " FOR UPDATE"
	}
	var raw string
	if err = conn.QueryRowContext(ctx, query).Scan(&raw); err != nil {
		return err
	}
	state := newState()
	if err = json.Unmarshal([]byte(raw), state); err != nil {
		return fmt.Errorf("decode persisted state: %w", err)
	}
	if state.Users == nil || state.Sessions == nil || state.Settings == nil || state.Docs == nil {
		return errors.New("incomplete persisted database state")
	}
	if err = fn(state); err != nil {
		return err
	}
	if write {
		var encoded []byte
		if encoded, err = json.Marshal(state); err != nil {
			return err
		}
		query = "UPDATE control_state SET payload=?,revision=revision+1 WHERE id=1"
		if s.dialect == "postgres" {
			query = "UPDATE control_state SET payload=$1,revision=revision+1 WHERE id=1"
		}
		if _, err = conn.ExecContext(ctx, query, string(encoded)); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
}

func LoadDoc[T any](s *State, collection, id string) (v T, ok bool) {
	raw, found := s.Docs[collection][id]
	if !found {
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, false
	}
	return v, true
}

func SaveDoc(s *State, collection, id string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if s.Docs[collection] == nil {
		s.Docs[collection] = map[string]json.RawMessage{}
	}
	s.Docs[collection][id] = raw
	return nil
}

func DeleteDoc(s *State, collection, id string) { delete(s.Docs[collection], id) }

func ListDocs[T any](s *State, collection string) []T {
	items := make([]T, 0, len(s.Docs[collection]))
	for id := range s.Docs[collection] {
		if v, ok := LoadDoc[T](s, collection, id); ok {
			items = append(items, v)
		}
	}
	return items
}

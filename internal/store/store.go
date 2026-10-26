// Package store holds all database access. It is the only package that
// writes SQL; everything else works with the types in models.go.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// driverName is the database/sql driver registered by driver_sqlite.go.
const driverName = "sqlite"

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
	// requestsRev changes whenever a request or an on-the-way status does,
	// so apps can ask cheaply whether to reload (see RequestsRevision).
	requestsRev atomic.Int64
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// WAL allows concurrent readers; writers wait on busy_timeout.
	db.SetMaxOpenConns(8)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrations table: %w", err)
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		v, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: bad version prefix", base)
		}
		var applied int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, v).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", base, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// NewID returns a random 128-bit hex ID.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func now() int64 { return time.Now().Unix() }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullPositive(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}

// nullNumber stores -1 as NULL and keeps 0 (season 0 is Specials).
func nullNumber(n int) any {
	if n < 0 {
		return nil
	}
	return n
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// placeholders returns "?, ?, ?" for n arguments.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// Snapshot writes a consistent copy of the whole database to path (which
// must not exist yet), while the server keeps running: SQLite's VACUUM
// INTO, which also leaves out free pages.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(path, "'", "''")+`'`)
	return err
}

// SchemaVersion is the newest migration this build knows, which a
// database made by it may have applied.
func SchemaVersion() int {
	names, _ := fs.Glob(migrationsFS, "migrations/*.sql")
	top := 0
	for _, name := range names {
		v, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, "migrations/"), "_", 2)[0])
		if err == nil && v > top {
			top = v
		}
	}
	return top
}

// Counts are what a backup shows about the library it holds.
type Counts struct {
	Users     int `json:"users"`
	Libraries int `json:"libraries"`
	Titles    int `json:"titles"`
	Watched   int `json:"watched"`
}

// CountAll counts accounts, libraries, titles (movies and shows) and
// titles marked watched in the database.
func (s *Store) CountAll(ctx context.Context) (Counts, error) {
	return countAll(ctx, s.db)
}

// CountFile is CountAll for a database file, opened read-only, and its
// schema version (the newest migration applied).
func CountFile(ctx context.Context, path string) (Counts, int, error) {
	db, err := sql.Open(driverName, "file:"+path+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return Counts{}, 0, err
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return Counts{}, 0, fmt.Errorf("not a Kinara database: %w", err)
	}
	c, err := countAll(ctx, db)
	return c, v, err
}

func countAll(ctx context.Context, db *sql.DB) (Counts, error) {
	var c Counts
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM libraries),
		(SELECT COUNT(*) FROM items WHERE kind IN ('movie', 'show')),
		(SELECT COUNT(*) FROM watch_state WHERE completed = 1)`).Scan(&c.Users, &c.Libraries, &c.Titles, &c.Watched)
	return c, err
}

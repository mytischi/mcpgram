package storage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps SQLite access and implements persistence and search for mcpgram.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at dbPath, configures WAL and PRAGMAs,
// and applies any pending embedded SQL migrations.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("database path must not be empty")
	}

	if dbPath != ":memory:" {
		dir := filepath.Dir(dbPath)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create database directory %q: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}

	// Single connection keeps SQLite writes serialized in-process without SQLITE_BUSY
	// and ensures :memory: databases remain shared across all queries.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping verifies database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

type migrationFile struct {
	version int
	name    string
	sql     string
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return fmt.Errorf("invalid migration filename %q: expected NNN_name.sql", e.Name())
		}
		ver, err := strconv.Atoi(prefix)
		if err != nil || ver <= 0 {
			return fmt.Errorf("invalid migration version in %q", e.Name())
		}
		raw, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		files = append(files, migrationFile{
			version: ver,
			name:    e.Name(),
			sql:     string(raw),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].version < files[j].version
	})

	for _, mf := range files {
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE version = ?`, mf.version,
		).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("check migration %d: %w", mf.version, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", mf.version, err)
		}
		if _, err := tx.ExecContext(ctx, mf.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("execute migration %s: %w", mf.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`,
			mf.version, time.Now().UTC().Unix(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", mf.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", mf.version, err)
		}
	}

	return nil
}

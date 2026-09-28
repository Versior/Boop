// Package store owns the SQLite database: opening it with the documented
// pragmas and applying the embedded migrations.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure Go SQLite driver, no cgo
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"

// Foreign keys, WAL and a busy timeout are required by docs/DATABASE.md.
// The pool is capped at one connection: the supported deployment is a single
// process, so serialising every statement removes lock contention entirely
// (a CLI command such as `boop init-owner` still writes safely thanks to WAL
// plus the busy timeout). _txlock=immediate makes every transaction take the
// write lock up front instead of failing while upgrading a read transaction.
const (
	dsnParams    = "_foreign_keys=1&_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_txlock=immediate"
	maxOpenConns = 1
)

// Open creates the parent directory when needed and returns a pool configured
// for the single-process deployment.
func Open(path string) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: database path must not be empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create data directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+dsnParams)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect %s: %w", path, err)
	}
	return db, nil
}

// Migrate applies every embedded migration that has not been applied yet, each
// in its own transaction, in ascending version order.
func Migrate(db *sql.DB) error {
	if db == nil {
		return errors.New("store: migrate: nil database")
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := db.Exec(schemaMigrationsDDL); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	applied, err := appliedVersions(db)
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		if applied[migration.version] {
			continue
		}
		if err := applyMigration(db, migration); err != nil {
			return err
		}
	}
	return nil
}

const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
)`

func applyMigration(db *sql.DB, migration migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin migration %d: %w", migration.version, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(migration.sql); err != nil {
		return fmt.Errorf("store: migration %s: %w", migration.name, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations(version, applied_at) VALUES(?, strftime('%Y-%m-%dT%H:%M:%SZ','now'))`,
		migration.version,
	); err != nil {
		return fmt.Errorf("store: record migration %s: %w", migration.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", migration.name, err)
	}
	return nil
}

// CurrentVersion reports the highest applied migration version, or 0 when the
// migrations table does not exist yet.
func CurrentVersion(ctx context.Context, db *sql.DB) (int, error) {
	if db == nil {
		return 0, errors.New("store: current version: nil database")
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	return version, nil
}

// LatestVersion reports the highest embedded migration version.
func LatestVersion() int {
	migrations, err := loadMigrations()
	if err != nil || len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

// Ready reports whether the database answers and every embedded migration has
// been applied. The HTTP readiness probe uses it.
func Ready(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("store: ready: nil database")
	}
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: database unreachable: %w", err)
	}
	current, err := CurrentVersion(ctx, db)
	if err != nil {
		return err
	}
	if latest := LatestVersion(); current < latest {
		return fmt.Errorf("store: schema version %d is behind %d", current, latest)
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := parseVersion(entry.Name())
		if err != nil {
			return nil, err
		}
		if previous, duplicate := seen[version]; duplicate {
			return nil, fmt.Errorf("store: migration version %d is used by both %s and %s", version, previous, entry.Name())
		}
		seen[version] = entry.Name()

		body, err := fs.ReadFile(migrationsFS, migrationsDir+"/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: entry.Name(), sql: string(body)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}

// parseVersion reads the numeric prefix of a migration file name such as
// 002_search.sql.
func parseVersion(name string) (int, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("store: migration %s must be named <version>_<name>.sql", name)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("store: migration %s has an invalid version prefix", name)
	}
	return version, nil
}

func appliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate schema_migrations: %w", err)
	}
	return applied, nil
}

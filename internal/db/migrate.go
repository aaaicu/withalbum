package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	// ErrSchemaTooNew means the database was migrated by a newer binary.
	// Downgrades are not supported; the server must refuse to start.
	ErrSchemaTooNew = errors.New("database schema is newer than this binary supports")
	// ErrIntegrityCheck means PRAGMA integrity_check did not report "ok".
	ErrIntegrityCheck = errors.New("database integrity check failed")
	// ErrForeignDatabase means a database at user_version 0 already contains
	// schema objects, so it is not an empty WithAlbum database.
	ErrForeignDatabase = errors.New("database at schema version 0 is not empty")
)

// Migration is one embedded schema step. Version N moves user_version from
// N-1 to N.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrations returns the embedded migrations in version order. Versions must
// be 1..N without gaps or duplicates.
func Migrations() ([]Migration, error) {
	return loadMigrations(migrationFS, "migrations")
}

func loadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var ms []Migration
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %q: name must start with a positive number and '_'", name)
		}
		body, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, Migration{Version: v, Name: name, SQL: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	for i, m := range ms {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migration %q: expected version %d (versions must be 1..N without gaps or duplicates)", m.Name, i+1)
		}
	}
	return ms, nil
}

// LatestVersion is the schema version this binary migrates to.
func LatestVersion() int {
	ms, err := Migrations()
	if err != nil {
		return 0
	}
	return len(ms)
}

// SchemaVersion returns PRAGMA user_version.
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// Migrate brings the database to LatestVersion.
//
//   - A database newer than this binary is refused with ErrSchemaTooNew
//     before anything is written.
//   - When migrations are pending, PRAGMA integrity_check must report "ok"
//     first, and a version-0 database must be empty (ErrForeignDatabase), so
//     a wrong or damaged file is never modified.
//   - Each migration runs in its own BEGIN IMMEDIATE transaction together with
//     the user_version bump and a foreign_key_check, so a failed step leaves
//     the database at the previous version.
//
// Re-running Migrate on an up-to-date database is a no-op, and concurrent
// Migrate calls on the same file are safe: each step re-reads user_version
// under the write lock.
func Migrate(ctx context.Context, db *sql.DB) error {
	return migrate(ctx, db, migrationFS, "migrations")
}

func migrate(ctx context.Context, db *sql.DB, fsys fs.FS, dir string) error {
	ms, err := loadMigrations(fsys, dir)
	if err != nil {
		return err
	}
	cur, err := SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if cur > len(ms) {
		return fmt.Errorf("%w: database version %d, binary supports up to %d", ErrSchemaTooNew, cur, len(ms))
	}
	if cur == len(ms) {
		return nil
	}
	if err := integrityCheck(ctx, db); err != nil {
		return err
	}
	for _, m := range ms[cur:] {
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration) (err error) {
	tx, err := db.BeginTx(ctx, nil) // BEGIN IMMEDIATE via _txlock
	if err != nil {
		return fmt.Errorf("migration %s: begin: %w", m.Name, err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	var cur int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	if cur >= m.Version {
		// Applied concurrently by another connection or process.
		return tx.Rollback()
	}
	if cur != m.Version-1 {
		return fmt.Errorf("migration %s: database at version %d, expected %d", m.Name, cur, m.Version-1)
	}
	if cur == 0 {
		// Checked under the write lock so a concurrent first migration is
		// not mistaken for a foreign database.
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
			return fmt.Errorf("migration %s: %w", m.Name, err)
		}
		if n > 0 {
			err = fmt.Errorf("%w: %d schema objects present", ErrForeignDatabase, n)
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	// user_version cannot be bound as a parameter; Version is an int.
	if _, err = tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.Version)); err != nil {
		return fmt.Errorf("migration %s: set user_version: %w", m.Name, err)
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("migration %s: foreign_key_check: %w", m.Name, err)
	}
	violation := rows.Next()
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("migration %s: foreign_key_check: %w", m.Name, err)
	}
	if violation {
		err = fmt.Errorf("migration %s: foreign key violations after applying", m.Name)
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: commit: %w", m.Name, err)
	}
	return nil
}

func integrityCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIntegrityCheck, err)
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return fmt.Errorf("%w: %v", ErrIntegrityCheck, err)
		}
		msgs = append(msgs, s)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrIntegrityCheck, err)
	}
	if len(msgs) == 1 && msgs[0] == "ok" {
		return nil
	}
	if len(msgs) > 5 {
		msgs = append(msgs[:5], "...")
	}
	return fmt.Errorf("%w: %s", ErrIntegrityCheck, strings.Join(msgs, "; "))
}

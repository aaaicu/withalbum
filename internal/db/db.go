// Package db opens the WithAlbum SQLite metadata database with the safety
// PRAGMAs applied to every connection, and migrates it to the schema version
// this binary was built for.
//
// The driver is modernc.org/sqlite (pure Go, no cgo) so that CGO_ENABLED=0
// cross builds for macOS, Linux and 32-bit ARM keep working.
//
// The database must live on a locally attached disk. WAL mode relies on
// shared-memory locking that is not reliable over network file systems
// (SMB/NFS NAS mounts); there it can corrupt the database.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// connPragmas run, in this order after busy_timeout, on every new connection
// the pool opens (the driver executes each _pragma DSN parameter per
// connection). foreign_keys and synchronous are per-connection settings, so
// setting them once on a single connection would not be enough.
var connPragmas = []string{
	"busy_timeout(5000)", // wait up to 5s for a lock instead of failing with SQLITE_BUSY
	"foreign_keys(1)",    // enforce REFERENCES; off by default in SQLite
	"journal_mode(WAL)",  // readers do not block the writer; persistent in the file
	"synchronous(FULL)",  // fsync the WAL on every commit: durability over speed
}

// maxOpenConns bounds the pool. Write serialization does not depend on it:
// see Open.
const maxOpenConns = 4

// ErrUnsafeConnection is returned when a connection does not report the
// required PRAGMA values (for example WAL could not be enabled).
var ErrUnsafeConnection = errors.New("sqlite connection does not have the required pragmas")

func dsn(path string) string {
	q := make([]string, 0, len(connPragmas)+1)
	for _, p := range connPragmas {
		q = append(q, "_pragma="+p)
	}
	// Every explicit transaction starts with BEGIN IMMEDIATE.
	q = append(q, "_txlock=immediate")
	return path + "?" + strings.Join(q, "&")
}

// Open opens (creating if needed) the SQLite database file at path. The
// parent directory must already exist; Open never creates directories, so a
// path on an unmounted disk fails instead of landing on the internal disk.
// Open does not migrate; call Migrate before use.
//
// Concurrency policy: one pool of up to maxOpenConns connections, with every
// explicit transaction started as BEGIN IMMEDIATE (_txlock=immediate). A write
// transaction therefore takes SQLite's single write lock up front, and
// concurrent writers queue on busy_timeout. With the default DEFERRED mode a
// transaction that reads first and writes later can fail with SQLITE_BUSY at
// the upgrade without the busy handler helping, which is the classic WAL
// deadlock. Plain reads outside transactions run concurrently on the other
// connections under WAL. Code that only reads should not open a transaction,
// since it would hold the write lock.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("db: empty path")
	}
	// The driver splits the DSN at the first '?', and a "file:" prefix would
	// switch SQLite to URI parsing. An absolute path without '?' avoids both.
	if strings.ContainsRune(path, '?') {
		return nil, fmt.Errorf("db: path must not contain '?': %q", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	sqlDB, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", abs, err)
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxOpenConns)

	conn, err := sqlDB.Conn(context.Background())
	if err == nil {
		err = CheckConn(context.Background(), conn)
		conn.Close()
	}
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: open %s: %w", abs, err)
	}
	return sqlDB, nil
}

// CheckConn verifies that conn has the PRAGMA values Open requires.
func CheckConn(ctx context.Context, conn *sql.Conn) error {
	var fk, sync, busy int
	var mode string
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
		return err
	}
	// synchronous: 2 = FULL.
	if fk != 1 || !strings.EqualFold(mode, "wal") || sync != 2 || busy != 5000 {
		return fmt.Errorf("%w: foreign_keys=%d journal_mode=%s synchronous=%d busy_timeout=%d",
			ErrUnsafeConnection, fk, mode, sync, busy)
	}
	return nil
}
